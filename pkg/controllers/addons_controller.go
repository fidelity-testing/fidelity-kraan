/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	helmopv1 "github.com/fluxcd/helm-operator/pkg/apis/helm.fluxcd.io/v1"
	"github.com/fluxcd/pkg/untar"
	sourcev1 "github.com/fluxcd/source-controller/api/v1alpha1"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kraanv1alpha1 "github.com/fidelity/kraan/pkg/api/v1alpha1"
	"github.com/fidelity/kraan/pkg/internal/apply"
	layers "github.com/fidelity/kraan/pkg/internal/layers"
	utils "github.com/fidelity/kraan/pkg/internal/utils"
)

var (
	hrOwnerKey = ".owner"
	reconciler *AddonsLayerReconciler
	RootPath   = "/repos"
)

func init() {
	path, set := os.LookupEnv("REPOS_PATH")
	if set {
		RootPath = path
	}
}

// AddonsLayerReconciler reconciles a AddonsLayer object.
type AddonsLayerReconciler struct {
	client.Client
	Config   *rest.Config
	k8client kubernetes.Interface
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Context  context.Context
	Applier  apply.LayerApplier
}

// NewReconciler returns an AddonsLayerReconciler instance
func NewReconciler(config *rest.Config, client client.Client, logger logr.Logger,
	scheme *runtime.Scheme) (*AddonsLayerReconciler, error) {
	reconciler = &AddonsLayerReconciler{
		Config: config,
		Client: client,
		Log:    logger,
		Scheme: scheme,
	}
	reconciler.k8client = reconciler.getK8sClient()
	reconciler.Context = context.Background()
	var err error
	reconciler.Applier, err = apply.NewApplier(client, logger, scheme)
	return reconciler, err
}

func (r *AddonsLayerReconciler) getK8sClient() kubernetes.Interface {
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(r.Config)
	if err != nil {
		//TODO - Adjust error handling?
		panic(err.Error())
	}

	return clientset
}

func (r *AddonsLayerReconciler) processPrune(l layers.Layer) (statusReconciled bool, err error) {
	ctx := r.Context
	applier := r.Applier

	pruneIsRequired, hrs, err := applier.PruneIsRequired(ctx, l)
	if err != nil {
		return false, err
	} else if pruneIsRequired {
		l.SetStatusPruning()
		if pruneErr := applier.Prune(ctx, l, hrs); err != nil {
			return true, pruneErr
		}
		l.SetDelayedRequeue()
		return true, nil
	}
	return false, nil
}

func (r *AddonsLayerReconciler) processApply(l layers.Layer) (statusReconciled bool, err error) {
	ctx := r.Context
	applier := r.Applier

	applyIsRequired, err := applier.ApplyIsRequired(ctx, l)
	if err != nil {
		return false, err
	} else if applyIsRequired {
		utils.Log(r.Log, 1, 1, "apply required", "Name", l.GetName(), "Spec", l.GetSpec(), "Status", l.GetFullStatus())
		if !l.DependenciesDeployed() {
			l.SetDelayedRequeue()
			return true, nil
		}

		l.SetStatusApplying()
		if applyErr := applier.Apply(ctx, l); err != nil {
			return true, applyErr
		}
		l.SetDelayedRequeue()
		return true, nil
	}
	return false, nil
}

func (r *AddonsLayerReconciler) checkSuccess(l layers.Layer) error {
	ctx := r.Context
	applier := r.Applier

	applyWasSuccessful, err := applier.ApplyWasSuccessful(ctx, l)
	if err != nil {
		// TODO - we might want to add some sort of error handling here
		return err
	}
	if !applyWasSuccessful {
		l.SetDelayedRequeue()
		return nil
	}
	l.SetStatusDeployed()
	return nil
}

func (r *AddonsLayerReconciler) processAddonLayer(l layers.Layer) error {
	utils.Log(r.Log, 1, 1, "processing", "Name", l.GetName(), "Status", l.GetStatus())

	if l.IsHold() {
		l.SetHold()
		return nil
	}

	if !l.CheckK8sVersion() {
		l.SetStatusK8sVersion()
		l.SetDelayedRequeue()
		return nil
	}

	layerStatusUpdated, err := r.processPrune(l)
	if err != nil {
		return err
	}
	if layerStatusUpdated {
		return nil
	}

	layerStatusUpdated, err = r.processApply(l)
	if err != nil {
		return err
	}
	if layerStatusUpdated {
		return nil
	}

	return r.checkSuccess(l)
}

func (r *AddonsLayerReconciler) updateRequeue(l layers.Layer, res *ctrl.Result, rerr *error) {
	if l.IsUpdated() {
		*rerr = r.update(r.Context, r.Log, l.GetAddonsLayer())
	}
	if l.NeedsRequeue() {
		if l.IsDelayed() {
			*res = ctrl.Result{Requeue: true, RequeueAfter: l.GetDelay()}
			return
		}
		*res = ctrl.Result{Requeue: true}
		return
	}
}

// Reconcile process AddonsLayers custom resources.
// +kubebuilder:rbac:groups=kraan.io,resources=addons,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kraan.io,resources=addons/status,verbs=get;update;patch
func (r *AddonsLayerReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := r.Context

	var addonsLayer *kraanv1alpha1.AddonsLayer = &kraanv1alpha1.AddonsLayer{}
	if err := r.Get(ctx, req.NamespacedName, addonsLayer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log := r.Log.WithValues("requestName", req.NamespacedName.Name)

	l := layers.CreateLayer(ctx, r.Client, r.k8client, log, addonsLayer)
	var rerr error = nil
	var res ctrl.Result = ctrl.Result{}
	//defer r.updateRequeue(l, &res, &rerr)
	err := r.processAddonLayer(l)
	if err != nil {
		l.StatusUpdate(kraanv1alpha1.FailedCondition, kraanv1alpha1.AddonsLayerFailedReason, err.Error())
	}
	r.updateRequeue(l, &res, &rerr)
	return res, rerr
}

func (r *AddonsLayerReconciler) update(ctx context.Context, log logr.Logger,
	a *kraanv1alpha1.AddonsLayer) error {
	if err := r.Status().Update(ctx, a); err != nil {
		log.Error(err, "unable to update AddonsLayer status")
		return err
	}

	return nil
}

func repoMapperFunc(a handler.MapObject) []reconcile.Request {
	kind := a.Object.GetObjectKind().GroupVersionKind()
	repoKind := sourcev1.GitRepositoryKind
	if kind.Kind != repoKind {
		// If this isn't a GitRepository object, return an empty list of requests
		reconciler.Log.Error(fmt.Errorf("unexpected object kind: %s, only %s supported", kind, sourcev1.GitRepositoryKind),
			"unexpected kind, continuing")
		//return []reconcile.Request{}
	}
	repo, ok := a.Object.(*sourcev1.GitRepository)
	if !ok {
		reconciler.Log.Error(fmt.Errorf("unable to cast object to GitRepository"), "skipping processing")
		return []reconcile.Request{}
	}

	dataPath, err := SyncRepo(repo)
	if err != nil {
		reconciler.Log.Error(err, "unable to sync repository")
		return []reconcile.Request{}
	}

	addonsList := &kraanv1alpha1.AddonsLayerList{}
	if err := reconciler.List(reconciler.Context, addonsList); err != nil {
		reconciler.Log.Error(err, "unable to list AddonsLayers")
		return []reconcile.Request{}
	}
	addons := []reconcile.Request{}
	for _, addon := range addonsList.Items {
		if addon.Spec.Source.Name == repo.ObjectMeta.Name && addon.Spec.Source.NameSpace == repo.ObjectMeta.Namespace {
			addons = append(addons, reconcile.Request{NamespacedName: types.NamespacedName{Name: addon.Name, Namespace: ""}})
		}
		reconciler.Log.Info("adding layer to list", "layer", addon.Name)
		addonsPath := GetSourcePath(&addon)

		err := os.Link(addonsPath, fmt.Sprintf("%s/%s", dataPath, addon.Spec.Source.Path))
		if err != nil {
			reconciler.Log.Error(err, fmt.Sprintf("unable link to new data for addonsLayers: %s", addon.Name))
			os.Exit(1)
		}
	}
	return addons
}

func indexHelmReleaseByOwner(o runtime.Object) []string {
	hr, ok := o.(*helmopv1.HelmRelease)
	if !ok {
		return nil
	}
	owner := metav1.GetControllerOf(hr)
	if owner == nil {
		return nil
	}
	if owner.APIVersion != kraanv1alpha1.GroupVersion.String() || owner.Kind != "AddonsLayer" {
		return nil
	}
	log := ctrl.Log.WithName("hr sync")
	utils.Log(log, 1, 5, "HR associated with layer", "Layer Name", owner.Name, "HR", fmt.Sprintf("%s/%s", hr.GetNamespace(), hr.GetName()))

	return []string{owner.Name}
}

// SetupWithManager is used to setup the controller
func (r *AddonsLayerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	addonsLayer := &kraanv1alpha1.AddonsLayer{}
	hr := &helmopv1.HelmRelease{}

	if err := mgr.GetFieldIndexer().IndexField(r.Context, &helmopv1.HelmRelease{}, hrOwnerKey, indexHelmReleaseByOwner); err != nil {
		return fmt.Errorf("failed setting up FieldIndexer for HelmRelease owner: %w", err)
	}

	repoKind := &source.Kind{Type: &sourcev1.GitRepository{}}
	repoHandler := &handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(repoMapperFunc)}

	return ctrl.NewControllerManagedBy(mgr).
		For(addonsLayer).
		Owns(hr).
		Watches(repoKind, repoHandler).
		Complete(r)
}

/*
Copyright 2020 The Flux CD contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

func SyncRepo(repository *sourcev1.GitRepository) (string, error) {
	// set timeout for the reconciliation
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	log := reconciler.Log
	log.Info("New revision detected", "revision", repository.Status.Artifact.Revision)

	// create tmp dir
	tmpDir, err := ioutil.TempDir("", fmt.Sprintf("%s/%s-*", repository.Name, repository.Status.Artifact.Revision))
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir, error: %w", err)
	}

	// download and extract artifact
	summary, err := fetchArtifact(ctx, repository, tmpDir)
	if err != nil {
		return "", err
	}
	log.Info("fetched artifact", "summary", summary)
	// list artifact content
	files, err := ioutil.ReadDir(tmpDir)
	if err != nil {
		return "", fmt.Errorf("faild to list files, error: %w", err)
	}
	for _, file := range files {
		log.Info("unpacked", "file", file)
	}
	return tmpDir, nil
}

func fetchArtifact(ctx context.Context, repository *sourcev1.GitRepository, dir string) (string, error) {
	if repository.Status.Artifact == nil {
		return "", fmt.Errorf("repository %s does not containt an artifact", repository.Name)
	}

	url := repository.Status.Artifact.URL

	// for local run:
	// kubectl -n gitops-system port-forward svc/source-controller 8080:80
	// export SOURCE_HOST=localhost:8080
	if hostname := os.Getenv("SOURCE_HOST"); hostname != "" {
		url = fmt.Sprintf("http://%s/gitrepository/%s/%s/latest.tar.gz", hostname, repository.Namespace, repository.Name)
	}

	// download the tarball
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request, error: %w", err)
	}

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("failed to download artifact from %s, error: %w", url, err)
	}
	defer resp.Body.Close()

	// check response
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("faild to download artifact, status: %s", resp.Status)
	}

	// extract
	summary, err := untar.Untar(resp.Body, dir)
	if err != nil {
		return "", fmt.Errorf("faild to untar artifact, error: %w", err)
	}

	return summary, nil
}
