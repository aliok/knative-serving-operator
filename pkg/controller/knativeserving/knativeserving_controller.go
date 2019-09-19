package knativeserving

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	mf "github.com/jcrossley3/manifestival"
	maistrav1 "github.com/maistra/istio-operator/pkg/apis/maistra/v1"
	servingv1alpha1 "github.com/openshift-knative/knative-serving-operator/pkg/apis/serving/v1alpha1"
	"github.com/openshift-knative/knative-serving-operator/pkg/controller/knativeserving/common"
	"github.com/openshift-knative/knative-serving-operator/version"

	"github.com/operator-framework/operator-sdk/pkg/predicate"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	operand = "knative-serving"
)

var (
	knativeVersion = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "knative_serving_version",
		Help: "Installed knative serving version info",
	}, []string{"version"})
	filename = flag.String("filename", "deploy/resources",
		"The filename containing the YAML resources to apply")
	recursive = flag.Bool("recursive", false,
		"If filename is a directory, process all manifests recursively")
	log = logf.Log.WithName("controller_knativeserving")
	// Platform-specific behavior to affect the installation
	platforms common.Platforms
)

func init() {
	// Metrics have to be registered to expose:
	metrics.Registry.MustRegister(
		knativeVersion,
	)
}

// Add creates a new KnativeServing Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileKnativeServing{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller.  All injections (e.g. InjectClient) are performed after this call to controller.New()	c, err := controller.New("knativeserving-controller", mgr, controller.Options{Reconciler: r})
	c, err := controller.New("knativeserving-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Add watchers by extensions
	if err := r.(*ReconcileKnativeServing).extensions.AddWatchers(c, mgr); err != nil {
		return err
	}

	// Watch for changes to primary resource KnativeServing
	err = c.Watch(&source.Kind{Type: &servingv1alpha1.KnativeServing{}}, &handler.EnqueueRequestForObject{}, predicate.GenerationChangedPredicate{})
	if err != nil {
		return err
	}

	// Watch child deployments for availability
	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &servingv1alpha1.KnativeServing{},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileKnativeServing{}

// ReconcileKnativeServing reconciles a KnativeServing object
type ReconcileKnativeServing struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client     client.Client
	scheme     *runtime.Scheme
	config     mf.Manifest
	extensions *common.Extensions
}

// Create manifestival resources and KnativeServing, if necessary
func (r *ReconcileKnativeServing) InjectClient(c client.Client) error {
	m, err := mf.NewManifest(*filename, *recursive, c)
	if err != nil {
		return err
	}
	r.config = m

	// execute extend functions
	ext, err := platforms.Extend(r.client, r.scheme, &r.config)
	if err != nil {
		return err
	}
	r.extensions = &ext
	return nil
}

// Reconcile reads that state of the cluster for a KnativeServing object and makes changes based on the state read
// and what is in the KnativeServing.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileKnativeServing) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling KnativeServing")

	// Fetch the KnativeServing instance
	instance := &servingv1alpha1.KnativeServing{}
	if err := r.client.Get(context.TODO(), request.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			if isInteresting(request) {
				r.config.DeleteAll()
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !isInteresting(request) {
		return reconcile.Result{}, r.ignore(instance)
	}

	stages := []func(*servingv1alpha1.KnativeServing) error{
		r.initStatus,
		r.checkDependencies,
		r.install,
		r.checkDeployments,
		r.deleteObsoleteResources,
	}

	for _, stage := range stages {
		if err := stage(instance); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

// Initialize status conditions
func (r *ReconcileKnativeServing) initStatus(instance *servingv1alpha1.KnativeServing) error {
	if len(instance.Status.Conditions) == 0 {
		instance.Status.InitializeConditions()
		if err := r.updateStatus(instance); err != nil {
			return err
		}
	}
	return nil
}

// Update the status subresource
func (r *ReconcileKnativeServing) updateStatus(instance *servingv1alpha1.KnativeServing) error {

	// Account for https://github.com/kubernetes-sigs/controller-runtime/issues/406
	gvk := instance.GroupVersionKind()
	defer instance.SetGroupVersionKind(gvk)

	if err := r.client.Status().Update(context.TODO(), instance); err != nil {
		return err
	}
	return nil
}

// Apply the embedded resources
func (r *ReconcileKnativeServing) install(instance *servingv1alpha1.KnativeServing) error {
	defer r.updateStatus(instance)

	err := r.config.Transform(r.extensions.Transform(instance, r.scheme)...)
	if err == nil {
		err = r.extensions.PreInstall(instance)
		if err == nil {
			err = r.config.ApplyAll()
			if err == nil {
				err = r.extensions.PostInstall(instance)
			}
		}
	}
	if err != nil {
		instance.Status.MarkInstallFailed(err.Error())
		return err
	}

	// Update status
	instance.Status.Version = version.Version
	log.Info("Install succeeded", "version", version.Version)
	instance.Status.MarkInstallSucceeded()
	r.exposeMetrics()
	return nil
}

// Expose metrics for installed knative serving operator
func (r *ReconcileKnativeServing) exposeMetrics() {
	log.Info("expose metrics for installed knative serving")
	knativeVersion.WithLabelValues(version.Version).Set(float64(time.Now().Unix()))
}

// Check for all deployments available
func (r *ReconcileKnativeServing) checkDeployments(instance *servingv1alpha1.KnativeServing) error {
	defer r.updateStatus(instance)
	available := func(d *appsv1.Deployment) bool {
		for _, c := range d.Status.Conditions {
			if c.Type == appsv1.DeploymentAvailable && c.Status == v1.ConditionTrue {
				return true
			}
		}
		return false
	}
	deployment := &appsv1.Deployment{}
	for _, u := range r.config.Resources {
		if u.GetKind() == "Deployment" {
			key := client.ObjectKey{Namespace: u.GetNamespace(), Name: u.GetName()}
			if err := r.client.Get(context.TODO(), key, deployment); err != nil {
				instance.Status.MarkDeploymentsNotReady()
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			if !available(deployment) {
				instance.Status.MarkDeploymentsNotReady()
				return nil
			}
		}
	}
	log.Info("All deployments are available")
	instance.Status.MarkDeploymentsAvailable()
	return nil
}

// Check for all dependencies
func (r *ReconcileKnativeServing) checkDependencies(instance *servingv1alpha1.KnativeServing) error {
	defer r.updateStatus(instance)
	checkError := func(err error, instance *servingv1alpha1.KnativeServing) error {
		msg := fmt.Sprintf("Istio not detected; please install ServiceMesh")
		instance.Status.MarkDependencyMissing(msg)
		log.Error(err, msg)
		return err
	}
	smmr := &maistrav1.ServiceMeshMemberRoll{}
	if err := r.client.Get(context.TODO(), client.ObjectKey{Namespace: "istio-system", Name: "default"}, smmr); err != nil {
		return checkError(err, instance)
	}
	found := func(smmr *maistrav1.ServiceMeshMemberRoll) bool {
		for _, member := range smmr.Status.ConfiguredMembers {
			if member == "knative-serving" {
				return true
			}
		}
		return false
	}
	if !found(smmr) {
		return checkError(errors.New("knative-serving namespace is not a configured member in serviceMeshMemberRoll"), instance)
	}
	log.Info("All dependencies are installed")
	instance.Status.MarkDependenciesInstalled()
	return nil
}

// Delete obsolete resources from previous versions
func (r *ReconcileKnativeServing) deleteObsoleteResources(instance *servingv1alpha1.KnativeServing) error {
	// istio-system resources from 0.3
	resource := &unstructured.Unstructured{}
	resource.SetNamespace("istio-system")
	resource.SetName("knative-ingressgateway")
	resource.SetAPIVersion("v1")
	resource.SetKind("Service")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	resource.SetAPIVersion("apps/v1")
	resource.SetKind("Deployment")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	resource.SetAPIVersion("autoscaling/v1")
	resource.SetKind("HorizontalPodAutoscaler")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	// config-controller from 0.5
	resource.SetNamespace(instance.GetNamespace())
	resource.SetName("config-controller")
	resource.SetAPIVersion("v1")
	resource.SetKind("ConfigMap")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	// resources from 0.7.1
	resource.SetName("config-certmanager")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	resource.SetName("networking-certmanager")
	resource.SetAPIVersion("apps/v1")
	resource.SetKind("Deployment")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	resource.SetNamespace("")
	resource.SetName("knative-serving-certmanager")
	resource.SetAPIVersion("rbac.authorization.k8s.io/v1")
	resource.SetKind("ClusterRole")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	return nil
}

// Because it's effectively cluster-scoped, we only care about a
// single, named resource: knative-serving/knative-serving
func isInteresting(request reconcile.Request) bool {
	return request.Namespace == operand && request.Name == operand
}

// Reflect our ignorance in the KnativeServing status
func (r *ReconcileKnativeServing) ignore(instance *servingv1alpha1.KnativeServing) (err error) {
	err = r.initStatus(instance)
	if err == nil {
		msg := fmt.Sprintf("The only KnativeServing resource that matters is %s/%s", operand, operand)
		instance.Status.MarkIgnored(msg)
		err = r.updateStatus(instance)
	}
	return
}
