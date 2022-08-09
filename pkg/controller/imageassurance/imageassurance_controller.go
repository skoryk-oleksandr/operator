// Copyright (c) 2022 Tigera, Inc. All rights reserved.

package imageassurance

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/tigera/operator/api/v1"
	"github.com/tigera/operator/pkg/common"
	"github.com/tigera/operator/pkg/controller/certificatemanager"
	"github.com/tigera/operator/pkg/controller/options"
	"github.com/tigera/operator/pkg/controller/status"
	"github.com/tigera/operator/pkg/controller/utils"
	"github.com/tigera/operator/pkg/controller/utils/imageset"
	"github.com/tigera/operator/pkg/dns"
	"github.com/tigera/operator/pkg/render"
	rcertificatemanagement "github.com/tigera/operator/pkg/render/certificatemanagement"
	rcimageassurance "github.com/tigera/operator/pkg/render/common/imageassurance"
	rmeta "github.com/tigera/operator/pkg/render/common/meta"
	"github.com/tigera/operator/pkg/render/imageassurance"
	"github.com/tigera/operator/pkg/tls/certificatemanagement"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_image_assurance")

// service accounts created by kube-controller for image assurance components for API access
var apiTokenServiceAccounts = []string{imageassurance.ScannerAPIAccessServiceAccountName, imageassurance.PodWatcherAPIAccessServiceAccountName}

// Add creates a new ImageAssurance Controller and adds it to the Manager.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager, opts options.AddOptions) error {
	var licenseAPIReady = &utils.ReadyFlag{}

	reconciler := newReconciler(mgr, opts, licenseAPIReady)

	c, err := controller.New("imageassurance-controller", mgr, controller.Options{Reconciler: reconcile.Reconciler(reconciler)})
	if err != nil {
		return err
	}

	k8sClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Error(err, "Failed to establish a connection to k8s")
		return err
	}

	go utils.WaitToAddLicenseKeyWatch(c, k8sClient, log, licenseAPIReady)

	return add(mgr, c)
}

// newReconciler returns a new *reconcile.Reconciler.
func newReconciler(mgr manager.Manager, opts options.AddOptions, licenseAPIReady *utils.ReadyFlag) reconcile.Reconciler {
	r := &ReconcileImageAssurance{
		client:          mgr.GetClient(),
		scheme:          mgr.GetScheme(),
		provider:        opts.DetectedProvider,
		status:          status.New(mgr.GetClient(), "imageassurance", opts.KubernetesVersion),
		clusterDomain:   opts.ClusterDomain,
		licenseAPIReady: licenseAPIReady,
	}
	r.status.Run(opts.ShutdownContext)
	return r
}

// add adds watches for resources that are available at startup.
func add(mgr manager.Manager, c controller.Controller) error {
	var err error

	// Watch for changes to primary resource ImageAssurance.
	err = c.Watch(&source.Kind{Type: &operatorv1.ImageAssurance{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	if err = imageset.AddImageSetWatch(c); err != nil {
		return fmt.Errorf("ImageAssurance-controller failed to watch ImageSet: %w", err)
	}

	if err = utils.AddNetworkWatch(c); err != nil {
		log.V(5).Info("Failed to create network watch", "err", err)
		return fmt.Errorf("ImageAssurance-controller failed to watch Tigera network resource: %v", err)
	}

	// Watch configmaps created for postgres in operator namespace.
	for _, cm := range []string{imageassurance.PGConfigMapName, rcimageassurance.ConfigurationConfigMapName} {
		if err = utils.AddConfigMapWatch(c, cm, common.OperatorNamespace()); err != nil {
			return fmt.Errorf("ImageAssurance-controller failed to watch ConfigMap %s: %v", cm, err)
		}
	}

	// Watch secrets created for postgres in operator namespace.
	for _, s := range []string{
		imageassurance.PGCertSecretName,
		imageassurance.APICertSecretName,
		imageassurance.PGAdminUserSecretName,
		imageassurance.TenantEncryptionKeySecretName,
		render.ManagerInternalTLSSecretName,
		certificatemanagement.CASecretName,
	} {
		if err = utils.AddSecretsWatch(c, s, common.OperatorNamespace()); err != nil {
			return fmt.Errorf("ImageAssurance-controller failed to watch Secret %s: %v", s, err)
		}
	}

	if err = utils.AddSecretsWatch(c, imageassurance.PGUserSecretName, imageassurance.NameSpaceImageAssurance); err != nil {
		return fmt.Errorf("ImageAssurance-controller failed to watch Secret %s: %v", imageassurance.PGUserSecretName, err)
	}

	// watch for service accounts created in operator namespace by kube-controllers for image assurance.
	for _, sa := range apiTokenServiceAccounts {
		if err = utils.AddServiceAccountWatch(c, sa, common.OperatorNamespace()); err != nil {
			return fmt.Errorf("ImageAssurance-controller failed to watch ServiceAccount %s: %v", sa, err)
		}
	}

	if err = utils.AddJobWatch(c, imageassurance.ResourceNameImageAssuranceDBMigrator, imageassurance.NameSpaceImageAssurance); err != nil {
		return fmt.Errorf("ImageAssurance-controller failed to watch Job %s: %v", imageassurance.ResourceNameImageAssuranceDBMigrator, err)
	}

	for _, role := range []string{imageassurance.PodWatcherClusterRoleName, imageassurance.ScannerClusterRoleName, imageassurance.AdmissionControllerAPIClusterRoleName} {
		if err = utils.AddClusterRoleWatch(c, role); err != nil {
			return fmt.Errorf("ImageAssurance-controller failed to watch Cluster role %s: %v", role, err)
		}
	}
	// Watch for changes to authentication
	err = c.Watch(&source.Kind{Type: &operatorv1.Authentication{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return fmt.Errorf("ImageAssurance-controller failed to watch resource: %w", err)
	}

	return nil
}

// Blank assignment to verify that ReconcileImageAssurance implements reconcile.Reconciler.
var _ reconcile.Reconciler = &ReconcileImageAssurance{}

// ReconcileImageAssurance reconciles a ImageAssurance object.
type ReconcileImageAssurance struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver.
	client          client.Client
	scheme          *runtime.Scheme
	provider        operatorv1.Provider
	status          status.StatusManager
	clusterDomain   string
	licenseAPIReady *utils.ReadyFlag
}

// Reconcile reads that state of the cluster for a ImageAssurance object and makes changes
// based on the state read and what is in the ImageAssurance.Spec.
func (r *ReconcileImageAssurance) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling ImageAssurance")

	ia, err := utils.GetImageAssurance(ctx, r.client)

	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			reqLogger.Info("ImageAssurance object not found")

			r.status.OnCRNotFound()
			return reconcile.Result{}, nil
		}
		reqLogger.Error(err, "Error querying for ImageAssurance")
		r.status.SetDegraded("Error querying for ImageAssurance", err.Error())
		return reconcile.Result{}, err
	}
	r.status.OnCRFound()
	variant, installation, err := utils.GetInstallation(ctx, r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Error(err, "Installation not found")
			r.status.SetDegraded("Installation not found", err.Error())
			return reconcile.Result{}, nil
		}
		reqLogger.Error(err, "Error querying installation")
		r.status.SetDegraded("Error querying installation", err.Error())
		return reconcile.Result{}, err
	}

	pullSecrets, err := utils.GetNetworkingPullSecrets(installation, r.client)
	if err != nil {
		reqLogger.Error(err, "Error retrieving image pull secrets")
		r.status.SetDegraded("Error retrieving image pull secrets", err.Error())
		return reconcile.Result{}, err
	}

	configurationConfigMap, err := utils.GetImageAssuranceConfigurationConfigMap(r.client)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Error(err, fmt.Sprintf("%s ConfigMap not found", rcimageassurance.ConfigurationConfigMapName))
			r.status.SetDegraded(fmt.Sprintf("%s ConfigMap not found", rcimageassurance.ConfigurationConfigMapName), err.Error())
			return reconcile.Result{}, nil
		}

		reqLogger.Error(err, "Error retrieving image assurance configuration")
		r.status.SetDegraded("Error retrieving image assurance configuration", err.Error())
		return reconcile.Result{}, err
	}

	pgConfig, err := getPGConfig(r.client)
	if err != nil {
		reqLogger.Error(err, "Error retrieving postgres configuration")
		r.status.SetDegraded("Error retrieving postgres configuration", err.Error())
		return reconcile.Result{}, err
	}

	pgUserSecret, err := getOrCreatePGUserSecret(r.client, configurationConfigMap.Data[rcimageassurance.ConfigurationConfigMapOrgIDKey])
	if err != nil {
		reqLogger.Error(err, "Error retrieving postgres user secret")
		r.status.SetDegraded("Error retrieving postgres secret", err.Error())
		return reconcile.Result{}, err
	}

	pgAdminUserSecret, err := getAdminPGUserSecret(r.client)
	if err != nil {
		reqLogger.Error(err, "Error retrieving postgres admin user secret")
		r.status.SetDegraded("Error retrieving postgres admin secret", err.Error())
		return reconcile.Result{}, err
	}

	pgCertSecret, err := getPGCertSecret(r.client)
	if err != nil {
		reqLogger.Error(err, "Error retrieving postgres cert secret")
		r.status.SetDegraded("Error retrieving postgres cert secret", err.Error())
		return reconcile.Result{}, err
	}

	certificateManager, err := certificatemanager.Create(r.client, installation, r.clusterDomain)
	if err != nil {
		log.Error(err, "unable to create the Tigera CA")
		r.status.SetDegraded("Unable to create the Tigera CA", err.Error())
		return reconcile.Result{}, err
	}
	internalMgrSecret, err := certificateManager.GetCertificate(r.client, render.ManagerInternalTLSSecretName, common.OperatorNamespace())

	if err != nil {
		reqLogger.Error(err, err.Error())
		r.status.SetDegraded("Error retrieving internal manager tls secret", err.Error())
		return reconcile.Result{}, err
	}

	trustedBundle := certificateManager.CreateTrustedBundle(internalMgrSecret)

	if internalMgrSecret == nil {
		reqLogger.Info("Waiting for internal manager tls certificate to be available")
		r.status.SetDegraded("Waiting for internal manager tls certificate to be available", "")
		return reconcile.Result{}, nil
	}

	tlsSecret, err := getAPICertSecret(r.client, r.clusterDomain)
	if err != nil {
		reqLogger.Error(err, err.Error())
		r.status.SetDegraded("Error in ensuring TLS certificate for image-assurance api", err.Error())
		return reconcile.Result{}, err
	}

	scannerAPIToken, err := getAPIAccessToken(r.client, imageassurance.ScannerAPIAccessServiceAccountName)
	if err != nil {
		reqLogger.Error(err, err.Error())
		r.status.SetDegraded("Error in retrieving scanner API access token", err.Error())
		return reconcile.Result{}, err
	}

	if scannerAPIToken == nil {
		reqLogger.Info("Waiting for scanner api access service account secret to be available")
		r.status.SetDegraded("Waiting for scanner api access service account secret to be available", "")
		return reconcile.Result{}, nil
	}

	podWatcherAPIToken, err := getAPIAccessToken(r.client, imageassurance.PodWatcherAPIAccessServiceAccountName)
	if err != nil {
		reqLogger.Error(err, err.Error())
		r.status.SetDegraded("Error in retrieving pod watcher API access token", err.Error())
		return reconcile.Result{}, err
	}

	if podWatcherAPIToken == nil {
		reqLogger.Info("Waiting for pod watcher api access service account secret to be available")
		r.status.SetDegraded("Waiting for pod watcher api access service account secret to be available", "")
		return reconcile.Result{}, nil
	}

	tenantEncryptionKeySecret, err := getTenantEncryptionKeySecret(r.client)
	if err != nil {
		reqLogger.Error(err, "Error retrieving tenant key")
		r.status.SetDegraded("Error retrieving tenant key", err.Error())
		return reconcile.Result{}, err
	}

	imageSet, err := imageset.GetImageSet(ctx, r.client, variant)
	if err != nil {
		reqLogger.Error(err, err.Error())
		r.status.SetDegraded("Error retrieving image set", err.Error())
		return reconcile.Result{}, err
	}

	if err = imageset.ValidateImageSet(imageSet); err != nil {
		reqLogger.Error(err, err.Error())
		r.status.SetDegraded("Error validating image set", err.Error())
		return reconcile.Result{}, err
	}

	ch := utils.NewComponentHandler(log, r.client, r.scheme, ia)

	// Fetch the Authentication spec. If present, we use to configure user authentication.
	authenticationCR, err := utils.GetAuthentication(ctx, r.client)
	if err != nil && !errors.IsNotFound(err) {
		r.status.SetDegraded("Error querying Authentication", err.Error())
		return reconcile.Result{}, err
	}

	if authenticationCR != nil && authenticationCR.Status.State != operatorv1.TigeraStatusReady {
		r.status.SetDegraded("Authentication is not ready", fmt.Sprintf("authenticationCR status: %s", authenticationCR.Status.State))
		return reconcile.Result{}, nil
	}

	kvc, err := utils.GetKeyValidatorConfig(ctx, r.client, authenticationCR, r.clusterDomain)
	if err != nil {
		log.Error(err, "Failed to process the authentication CR.")
		r.status.SetDegraded("Failed to process the authentication CR.", err.Error())
		return reconcile.Result{}, err
	}

	config := &imageassurance.Config{
		PullSecrets:               pullSecrets,
		Installation:              installation,
		OsType:                    rmeta.OSTypeLinux,
		ConfigurationConfigMap:    configurationConfigMap,
		PGConfig:                  pgConfig,
		PGAdminUserSecret:         pgAdminUserSecret,
		PGCertSecret:              pgCertSecret,
		PGUserSecret:              pgUserSecret,
		TLSSecret:                 tlsSecret,
		KeyValidatorConfig:        kvc,
		TenantEncryptionKeySecret: tenantEncryptionKeySecret,
		TrustedCertBundle:         trustedBundle,
		ScannerAPIAccessToken:     scannerAPIToken,
		PodWatcherAPIAccessToken:  podWatcherAPIToken,
	}

	components := []render.Component{
		render.NewPassthrough([]client.Object{tlsSecret}...),
		imageassurance.ImageAssurance(config),
		rcertificatemanagement.CertificateManagement(&rcertificatemanagement.Config{
			Namespace:       imageassurance.NameSpaceImageAssurance,
			ServiceAccounts: []string{imageassurance.ResourceNameImageAssuranceAPI},
			TrustedBundle:   trustedBundle,
		}),
	}

	if err = imageset.ApplyImageSet(ctx, r.client, variant, components...); err != nil {
		reqLogger.Error(err, "Error with images from ImageSet")
		r.status.SetDegraded("Error with images from ImageSet", err.Error())
		return reconcile.Result{}, err
	}

	for _, component := range components {
		if err := ch.CreateOrUpdateOrDelete(ctx, component, r.status); err != nil {
			reqLogger.Error(err, "Error creating / updating resource")
			r.status.SetDegraded("Error creating / updating resource", err.Error())
			return reconcile.Result{}, err
		}
	}

	// Clear the degraded bit since we've reached this far.
	r.status.ClearDegraded()

	if !r.status.IsAvailable() {
		// Schedule a kick to check again in the near future, hopefully by then things will be available.
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Everything is available - update the CRD status.
	ia.Status.State = operatorv1.TigeraStatusReady
	if err = r.client.Status().Update(ctx, ia); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// getOrCreatePGUserSecret returns the PostgreSQL user secret if it exists, and creates it if it doesn't.
func getOrCreatePGUserSecret(client client.Client, orgID string) (*corev1.Secret, error) {
	us := &corev1.Secret{}
	snn := types.NamespacedName{
		Name:      imageassurance.PGUserSecretName,
		Namespace: imageassurance.NameSpaceImageAssurance,
	}

	if err := client.Get(context.Background(), snn, us); err != nil {
		if errors.IsNotFound(err) {
			pass, err := utils.RandomPassword(16)
			if err != nil {
				return nil, err
			}
			us = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      imageassurance.PGUserSecretName,
					Namespace: imageassurance.NameSpaceImageAssurance,
				},
				Data: map[string][]byte{
					"username": []byte(orgID + "_user"),
					"password": []byte(pass),
				},
			}
			return us, nil
		}
		return nil, fmt.Errorf("failed to read secret %q: %s", imageassurance.PGUserSecretName, err)
	}

	if user, ok := us.Data[imageassurance.PGUserSecretKey]; !ok || len(user) == 0 {
		return nil, fmt.Errorf("expected secret %q to have a field named %q",
			imageassurance.PGUserSecretName, imageassurance.PGUserSecretKey)
	}

	if pass, ok := us.Data[imageassurance.PGUserPassKey]; !ok || len(pass) == 0 {
		return nil, fmt.Errorf("expected secret %q to have a field named %q",
			imageassurance.PGUserSecretName, imageassurance.PGUserPassKey)
	}

	return us, nil

}

// getAdminPGUserSecret returns the PostgreSQL admin user secret.
func getAdminPGUserSecret(client client.Client) (*corev1.Secret, error) {
	us := &corev1.Secret{}
	snn := types.NamespacedName{
		Name:      imageassurance.PGAdminUserSecretName,
		Namespace: common.OperatorNamespace(),
	}

	if err := client.Get(context.Background(), snn, us); err != nil {
		return nil, fmt.Errorf("failed to read secret %q: %s", imageassurance.PGAdminUserSecretName, err)
	}

	if user, ok := us.Data[imageassurance.PGUserSecretKey]; !ok || len(user) == 0 {
		return nil, fmt.Errorf("expected secret %q to have a field named %q",
			imageassurance.PGAdminUserSecretName, imageassurance.PGUserSecretKey)
	}

	if pass, ok := us.Data[imageassurance.PGUserPassKey]; !ok || len(pass) == 0 {
		return nil, fmt.Errorf("expected secret %q to have a field named %q",
			imageassurance.PGAdminUserSecretName, imageassurance.PGUserPassKey)
	}

	return us, nil

}

// getPGCertSecret returns the PostgreSQL server secret.
func getPGCertSecret(client client.Client) (*corev1.Secret, error) {
	cs := &corev1.Secret{}
	snn := types.NamespacedName{
		Name:      imageassurance.PGCertSecretName,
		Namespace: common.OperatorNamespace(),
	}

	if err := client.Get(context.Background(), snn, cs); err != nil {
		return nil, fmt.Errorf("failed to read secret %q: %s", imageassurance.PGCertSecretName, err)
	}

	if ca, ok := cs.Data[imageassurance.PGServerCAKey]; !ok || len(ca) == 0 {
		return nil, fmt.Errorf("expected secret %q to have a field named %q",
			imageassurance.PGCertSecretName, imageassurance.PGServerCAKey)
	}

	if key, ok := cs.Data[imageassurance.PGClientKeyKey]; !ok || len(key) == 0 {
		return nil, fmt.Errorf("expected secret %q to have a field named %q",
			imageassurance.PGCertSecretName, imageassurance.PGClientKeyKey)
	}

	if cert, ok := cs.Data[imageassurance.PGClientCertKey]; !ok || len(cert) == 0 {
		return nil, fmt.Errorf("expected secret %q to have a field named %q",
			imageassurance.PGCertSecretName, imageassurance.PGClientCertKey)
	}

	return cs, nil
}

// getAPICertSecret returns the image assurance api tls secret.
// It returns secret if available otherwise creates a new tls secret and returns it.
func getAPICertSecret(client client.Client, clusterDomain string) (*corev1.Secret, error) {
	// note that if secret is not found, ValidateCertPair returns nil, nil
	secret, err := utils.ValidateCertPair(client, common.OperatorNamespace(), imageassurance.APICertSecretName,
		corev1.TLSPrivateKeyKey, corev1.TLSCertKey)

	if err != nil {
		return nil, err
	}

	// If secret is found, ensure it has valid DNS names, note that if secret is nil EnsureCertificateSecret creates a new one.
	svcDNSNames := dns.GetServiceDNSNames(imageassurance.ResourceNameImageAssuranceAPI, imageassurance.NameSpaceImageAssurance, clusterDomain)
	secret, _, err = utils.EnsureCertificateSecret(
		imageassurance.APICertSecretName, secret, corev1.TLSPrivateKeyKey, corev1.TLSCertKey, rmeta.DefaultCertificateDuration, svcDNSNames...,
	)

	if err != nil {
		return nil, fmt.Errorf("error ensuring TLS certificate exists and has valid DNS names %q: %s", render.ManagerInternalTLSSecretName, err)
	}

	return secret, nil
}

// getPGConfig returns configuration to connect to PostgreSQL.
func getPGConfig(client client.Client) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	nn := types.NamespacedName{
		Name:      imageassurance.PGConfigMapName,
		Namespace: common.OperatorNamespace(),
	}

	if err := client.Get(context.Background(), nn, cm); err != nil {
		return nil, fmt.Errorf("failed to read secret %q: %s", imageassurance.PGConfigMapName, err)
	}

	if host, ok := cm.Data[imageassurance.PGConfigHostKey]; !ok || len(host) == 0 {
		return nil, fmt.Errorf("expected configmap %q to have a field named %q",
			imageassurance.PGConfigMapName, imageassurance.PGConfigHostKey)
	}

	if name, ok := cm.Data[imageassurance.PGConfigNameKey]; !ok || len(name) == 0 {
		return nil, fmt.Errorf("expected configmap %q to have a field named %q",
			imageassurance.PGConfigMapName, imageassurance.PGConfigNameKey)
	}

	if port, ok := cm.Data[imageassurance.PGConfigPortKey]; !ok || len(port) == 0 {
		return nil, fmt.Errorf("expected configmap %q to have a field named %q",
			imageassurance.PGConfigMapName, imageassurance.PGConfigPortKey)
	}

	if orgName, ok := cm.Data[imageassurance.PGConfigOrgNameKey]; !ok || len(orgName) == 0 {
		return nil, fmt.Errorf("expected configmap %q to have a field named %q",
			imageassurance.PGConfigMapName, imageassurance.PGConfigOrgNameKey)
	}

	return cm, nil
}

// getTenantEncryptionKeySecret returns the image assurance tenant key.
func getTenantEncryptionKeySecret(client client.Client) (*corev1.Secret, error) {
	cs := &corev1.Secret{}
	snn := types.NamespacedName{
		Name:      imageassurance.TenantEncryptionKeySecretName,
		Namespace: common.OperatorNamespace(),
	}

	if err := client.Get(context.Background(), snn, cs); err != nil {
		return nil, fmt.Errorf("failed to read secret %q: %s", imageassurance.TenantEncryptionKeySecretName, err)
	}

	if ca, ok := cs.Data[imageassurance.EncryptionKey]; !ok || len(ca) == 0 {
		return nil, fmt.Errorf("expected secret %q to have a field named %q",
			imageassurance.PGCertSecretName, imageassurance.EncryptionKey)
	}

	return cs, nil
}

// getAPIAccessToken returns the image assurance service account secret token created by kube-controllers.
// It takes in service account name and uses it to validate the existence of the service account and return the token if present.
func getAPIAccessToken(c client.Client, serviceAccountName string) ([]byte, error) {
	sa := &corev1.ServiceAccount{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      serviceAccountName,
		Namespace: common.OperatorNamespace(),
	}, sa); err != nil {
		return nil, err
	}

	if len(sa.Secrets) == 0 {
		return nil, nil
	}

	saSecret := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name:      sa.Secrets[0].Name,
		Namespace: common.OperatorNamespace(),
	}, saSecret); err != nil {
		return nil, err
	}

	return saSecret.Data["token"], nil
}
