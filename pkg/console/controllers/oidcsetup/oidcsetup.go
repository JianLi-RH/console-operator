package oidcsetup

import (
	"context"
	"fmt"
	"time"

	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	corev1 "k8s.io/api/core/v1"
	apiexensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiexensionsv1informers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	apiexensionsv1listers "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	appsv1informers "k8s.io/client-go/informers/apps/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/console-operator/pkg/api"
	"github.com/openshift/console-operator/pkg/console/status"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	deploymentsub "github.com/openshift/console-operator/pkg/console/subresource/deployment"
	utilsub "github.com/openshift/console-operator/pkg/console/subresource/util"
)

// oidcSetupController:
//
//	writes:
//	- authentication.config.openshift.io/cluster .status.oidcClients:
//		- componentName=console
//		- componentNamespace=openshift-console
//		- currentOIDCClients
//		- conditions:
//			- Available
//			- Progressing
//			- Degraded
//	- consoles.operator.openshift.io/cluster .status.conditions:
//		- type=OIDCClientConfigProgressing
//		- type=OIDCClientConfigDegraded
//		- type=AuthStatusHandlerProgressing
//		- type=AuthStatusHandlerDegraded
type oidcSetupController struct {
	operatorClient v1helpers.OperatorClient

	authnLister               configv1listers.AuthenticationLister
	crdLister                 apiexensionsv1listers.CustomResourceDefinitionLister
	consoleOperatorLister     operatorv1listers.ConsoleLister
	targetNSSecretsLister     corev1listers.SecretLister
	targetNSConfigMapLister   corev1listers.ConfigMapLister
	targetNSDeploymentsLister appsv1listers.DeploymentLister

	authStatusHandler *status.AuthStatusHandler
}

func NewOIDCSetupController(
	operatorClient v1helpers.OperatorClient,
	authnInformer configv1informers.AuthenticationInformer,
	authenticationClient configv1client.AuthenticationInterface,
	consoleOperatorInformer operatorv1informers.ConsoleInformer,
	crdInformer apiexensionsv1informers.CustomResourceDefinitionInformer,
	targetNSsecretsInformer corev1informers.SecretInformer,
	targetNSConfigMapInformer corev1informers.ConfigMapInformer,
	targetNSDeploymentsInformer appsv1informers.DeploymentInformer,
	recorder events.Recorder,
) factory.Controller {
	c := &oidcSetupController{
		operatorClient: operatorClient,

		authnLister:               authnInformer.Lister(),
		consoleOperatorLister:     consoleOperatorInformer.Lister(),
		crdLister:                 crdInformer.Lister(),
		targetNSSecretsLister:     targetNSsecretsInformer.Lister(),
		targetNSDeploymentsLister: targetNSDeploymentsInformer.Lister(),
		targetNSConfigMapLister:   targetNSConfigMapInformer.Lister(),

		authStatusHandler: status.NewAuthStatusHandler(authenticationClient, api.OpenShiftConsoleName, api.TargetNamespace, api.OpenShiftConsoleOperator),
	}
	return factory.New().
		WithSync(c.sync).
		ResyncEvery(wait.Jitter(time.Minute, 1.0)).
		WithFilteredEventsInformers(
			factory.NamesFilter("authentications.config.openshift.io"),
			crdInformer.Informer(),
		).
		WithInformers(
			authnInformer.Informer(),
			consoleOperatorInformer.Informer(),
			targetNSsecretsInformer.Informer(),
			targetNSDeploymentsInformer.Informer(),
			targetNSConfigMapInformer.Informer(),
		).
		ToController("OIDCSetupController", recorder.WithComponentSuffix("oidc-setup-controller"))
}

func (c *oidcSetupController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	statusHandler := status.NewStatusHandler(c.operatorClient)

	if shouldSync, err := c.handleManaged(ctx); err != nil {
		return err
	} else if !shouldSync {
		return nil
	}

	oidcClientsSchema, err := authnConfigHasOIDCFields(c.crdLister)
	if err != nil {
		return statusHandler.FlushAndReturn(err)
	}

	// the schema is feature-gating this controller, we assume API validation won't
	// allow authentication/cluster 'Type=OIDC' if the `.status.oidcClients` field
	// does not exist
	if !oidcClientsSchema {
		// reset all conditions set by this controller
		statusHandler.AddConditions(status.HandleProgressingOrDegraded("OIDCClientConfig", "", nil))
		statusHandler.AddConditions(status.HandleProgressingOrDegraded("AuthStatusHandler", "", nil))
		return statusHandler.FlushAndReturn(nil)
	}

	operatorConfig, err := c.consoleOperatorLister.Get(api.ConfigResourceName)
	if err != nil {
		return err
	}

	authnConfig, err := c.authnLister.Get(api.ConfigResourceName)
	if err != nil {
		return err
	}

	if authnConfig.Spec.Type != configv1.AuthenticationTypeOIDC {
		applyErr := c.authStatusHandler.Apply(ctx, authnConfig)
		statusHandler.AddConditions(status.HandleProgressingOrDegraded("AuthStatusHandler", "FailedApply", applyErr))

		// reset the other condition set by this controller
		statusHandler.AddConditions(status.HandleProgressingOrDegraded("OIDCClientConfig", "", nil))
		return statusHandler.FlushAndReturn(applyErr)
	}

	// we need to keep track of errors during the sync so that we can requeue
	// if any occur
	var errs []error
	syncErr := c.syncAuthTypeOIDC(ctx, syncCtx, statusHandler, operatorConfig, authnConfig)
	statusHandler.AddConditions(
		status.HandleProgressingOrDegraded(
			"OIDCClientConfig", "OIDCConfigSyncFailed",
			syncErr,
		),
	)
	if syncErr != nil {
		errs = append(errs, syncErr)
	}

	applyErr := c.authStatusHandler.Apply(ctx, authnConfig)
	statusHandler.AddConditions(status.HandleProgressingOrDegraded("AuthStatusHandler", "FailedApply", applyErr))
	if applyErr != nil {
		errs = append(errs, applyErr)
	}

	if len(errs) > 0 {
		return statusHandler.FlushAndReturn(factory.SyntheticRequeueError)
	}
	return statusHandler.FlushAndReturn(nil)
}

func (c *oidcSetupController) syncAuthTypeOIDC(
	ctx context.Context,
	controllerContext factory.SyncContext,
	statusHandler status.StatusHandler,
	operatorConfig *operatorv1.Console,
	authnConfig *configv1.Authentication,
) error {

	clientConfig := utilsub.GetOIDCClientConfig(authnConfig)
	if clientConfig == nil {
		c.authStatusHandler.WithCurrentOIDCClient("")
		c.authStatusHandler.Unavailable("OIDCClientConfig", "no OIDC client found")
		return nil
	}

	if len(clientConfig.ClientID) == 0 {
		return fmt.Errorf("no ID set on console's OIDC client")
	}
	c.authStatusHandler.WithCurrentOIDCClient(clientConfig.ClientID)

	if len(clientConfig.ClientSecret.Name) == 0 {
		c.authStatusHandler.Degraded("OIDCClientMissingSecret", "no client secret in the OIDC client config")
		return nil
	}

	clientSecret, err := c.targetNSSecretsLister.Secrets(api.TargetNamespace).Get("console-oauth-config")
	if err != nil {
		c.authStatusHandler.Degraded("OIDCClientSecretGet", err.Error())
		return err
	}

	if valid, msg, err := c.checkClientConfigStatus(authnConfig, clientSecret); err != nil {
		c.authStatusHandler.Degraded("DeploymentOIDCConfig", err.Error())
		return err

	} else if !valid {
		c.authStatusHandler.Progressing("DeploymentOIDCConfig", msg)
		return nil
	}

	c.authStatusHandler.Available("OIDCConfigAvailable", "")
	return nil
}

// checkClientConfigStatus checks whether the current client configuration is being currently in use,
// by looking at the deployment status. It checks whether the deployment is available and updated,
// and also whether the resource versions for the oauth secret and server CA trust configmap match
// the deployment.
func (c *oidcSetupController) checkClientConfigStatus(authnConfig *configv1.Authentication, clientSecret *corev1.Secret) (bool, string, error) {
	depl, err := c.targetNSDeploymentsLister.Deployments(api.OpenShiftConsoleNamespace).Get(api.OpenShiftConsoleDeploymentName)
	if err != nil {
		return false, "", err
	}

	deplAvailableUpdated := deploymentsub.IsAvailableAndUpdated(depl)
	if !deplAvailableUpdated {
		return false, "deployment unavailable or outdated", nil
	}

	if clientSecret.GetResourceVersion() != depl.ObjectMeta.Annotations["console.openshift.io/oauth-secret-version"] {
		return false, "client secret version not up to date in current deployment", nil
	}

	if len(authnConfig.Spec.OIDCProviders) > 0 {
		serverCAConfigName := authnConfig.Spec.OIDCProviders[0].Issuer.CertificateAuthority.Name
		if len(serverCAConfigName) == 0 {
			return deplAvailableUpdated, "", nil
		}

		serverCAConfig, err := c.targetNSConfigMapLister.ConfigMaps(api.OpenShiftConsoleNamespace).Get(serverCAConfigName)
		if err != nil {
			return false, "", err
		}

		if serverCAConfig.GetResourceVersion() != depl.ObjectMeta.Annotations["console.openshift.io/authn-ca-trust-config-version"] {
			return false, "OIDC provider CA version not up to date in current deployment", nil
		}
	}

	return deplAvailableUpdated, "", nil
}

// handleStatus returns whether sync should happen and any error encountering
// determining the operator's management state
// TODO: extract this logic to where it can be used for all controllers
func (c *oidcSetupController) handleManaged(ctx context.Context) (bool, error) {
	operatorSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return false, fmt.Errorf("failed to retrieve operator config: %w", err)
	}

	switch managementState := operatorSpec.ManagementState; managementState {
	case operatorv1.Managed:
		klog.V(4).Infoln("console is in a managed state.")
		return true, nil
	case operatorv1.Unmanaged:
		klog.V(4).Infoln("console is in an unmanaged state.")
		return false, nil
	case operatorv1.Removed:
		klog.V(4).Infoln("console has been removed.")
		return false, nil
	default:
		return false, fmt.Errorf("console is in an unknown state: %v", managementState)
	}
}

func authnConfigHasOIDCFields(crdLister apiexensionsv1listers.CustomResourceDefinitionLister) (bool, error) {
	authnCRD, err := crdLister.Get("authentications.config.openshift.io")
	if err != nil {
		return false, err
	}

	var authnV1Config *apiexensionsv1.CustomResourceDefinitionVersion
	for _, version := range authnCRD.Spec.Versions {
		if version.Name == "v1" && version.Served && version.Storage {
			authnV1Config = &version
			break
		}
	}

	if authnV1Config == nil {
		return false, fmt.Errorf("authentications.config.openshift.io is not served or stored as v1")
	}

	schema := authnV1Config.Schema.OpenAPIV3Schema
	_, clientsExist := schema.Properties["status"].Properties["oidcClients"]

	return clientsExist, nil

}
