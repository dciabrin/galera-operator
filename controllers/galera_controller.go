/*
Copyright 2022.

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
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"bytes"
	"context"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1 "github.com/dciabrin/galera-operator/api/v1"
)

// GaleraReconciler reconciles a Galera object
type GaleraReconciler struct {
	client.Client
	config       *rest.Config
	configClient configclient.Interface
	fullClient   kubernetes.Interface
	Log          logr.Logger
	Scheme       *runtime.Scheme
}

func execInPod(config *rest.Config, client kubernetes.Interface, namespace, pod, container string, cmd []string) (*bytes.Buffer, *bytes.Buffer, error) {
	req := client.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").Param("container", container)
	req.VersionedParams(
		&corev1.PodExecOptions{
			Command: cmd,
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     false,
		},
		scheme.ParameterCodec,
	)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return nil, nil, err
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})

	return &stdout, &stderr, err
}

func FindBestCandidate(status *databasev1.GaleraStatus) string {
	sortednodes := []string{}
	for node := range status.Attributes {
		sortednodes = append(sortednodes, node)
	}
	sort.Strings(sortednodes)

	bestnode := ""
	bestseqno := -1
	for _, node := range sortednodes {
		seqno := status.Attributes[node].Seqno
		intseqno, _ := strconv.Atoi(seqno)
		if intseqno >= bestseqno {
			bestnode = node
			bestseqno = intseqno
		}
	}
	return bestnode //"galera-0"
}

func BuildGcommURI(galera *databasev1.Galera) string {
	size := int(galera.Spec.Size)
	basename := galera.Name
	res := []string{}

	for i := 0; i < size; i++ {
		res = append(res, basename+"-"+strconv.Itoa(i)+"."+basename)
	}
	uri := "gcomm://" + strings.Join(res, ",")
	return uri // gcomm://galera-0.galera,galera-1.galera,galera-2.galera
}

func injectGcommURI(r *GaleraReconciler, galera *databasev1.Galera, node string, uri string) error {
	_, _, rc := execInPod(r.config, r.fullClient, galera.Namespace, node, "galera", []string{"/bin/bash", "-c", "echo '" + uri + "' > /tmp/gcomm_uri"})
	if rc != nil {
		return rc
	}
	attr := galera.Status.Attributes[node]
	attr.Gcomm = uri
	galera.Status.Attributes[node] = attr
	return nil
}

// generate rbac to get, list, watch, create, update and patch the galeras status the galera resource
// +kubebuilder:rbac:groups=database.example.com,resources=galeras,verbs=get;list;watch;create;update;patch;delete

// generate rbac to get, update and patch the galera status the galera/finalizers
// +kubebuilder:rbac:groups=database.example.com,resources=galeras/status,verbs=get;update;patch

// generate rbac to update the galera/finalizers
// +kubebuilder:rbac:groups=database.example.com,resources=galeras/finalizers,verbs=update

// generate rbac to get, list, watch, create, update, patch, and delete statefulsets
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete

// generate rbac to get,list, and watch pods
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

//+kubebuilder:rbac:groups="",resources=pods,verbs=list;get
//+kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Galera object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *GaleraReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("galera", req.NamespacedName)

	// Fetch the Galera instance
	galera := &databasev1.Galera{}
	err := r.Get(ctx, req.NamespacedName, galera)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("Galera resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get Galera")
		return ctrl.Result{}, err
	}

	// Check if the stateful already exists, if not create a new one
	found := &appsv1.StatefulSet{}
	err = r.Get(ctx, req.NamespacedName, found)
	if err != nil && errors.IsNotFound(err) {
		// Define a new StatefulSet
		dep := r.statefulSetForGalera(galera)
		log.Info("Creating a new StatefulSet", "StatefulSet.Namespace", dep.Namespace, "StatefulSet.Name", dep.Name)
		err = r.Create(ctx, dep)
		if err != nil {
			log.Error(err, "Failed to create new StatefulSet", "StatefulSet.Namespace", dep.Namespace, "StatefulSet.Name", dep.Name)
			return ctrl.Result{}, err
		}
		// StatefulSet created successfully - return and requeue
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Error(err, "Failed to get StatefulSet")
		return ctrl.Result{}, err
	}

	// Check if the headless service already exists, if not create a new one
	svcfound := &corev1.Service{}
	err = r.Get(ctx, req.NamespacedName, svcfound)
	if err != nil && errors.IsNotFound(err) {
		// Define a new headless service
		hsvc := r.headlessServiceForGalera(galera)
		log.Info("Creating a new headless Service", "StatefulSet.Namespace", hsvc.Namespace, "StatefulSet.Name", hsvc.Name)
		err = r.Create(ctx, hsvc)
		if err != nil {
			log.Error(err, "Failed to create new headless Service", "StatefulSet.Namespace", hsvc.Namespace, "StatefulSet.Name", hsvc.Name)
			return ctrl.Result{}, err
		}
		// StatefulSet created successfully - return and requeue
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Error(err, "Failed to get headless Service")
		return ctrl.Result{}, err
	}

	// log.Info("*******", "attr", galera.Status.Attributes)

	// Ensure the deployment size is the same as the spec
	size := galera.Spec.Size
	if *found.Spec.Replicas != size {
		found.Spec.Replicas = &size
		err = r.Update(ctx, found)
		if err != nil {
			log.Error(err, "Failed to update StatefulSet", "StatefulSet.Namespace", found.Namespace, "StatefulSet.Name", found.Name)
			return ctrl.Result{}, err
		}
		// Spec updated - return and requeue
		return ctrl.Result{Requeue: true}, nil
	}

	// List the pods for this galera's deployment
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(galera.Namespace),
		client.MatchingLabels(labelsForGalera(galera.Name)),
	}
	if err = r.List(ctx, podList, listOpts...); err != nil {
		log.Error(err, "Failed to list pods", "Galera.Namespace", galera.Namespace, "Galera.Name", galera.Name)
		return ctrl.Result{}, err
	}

	// Check if new pods have been detected
	podNames := getPodNames(podList.Items)

	if len(podNames) == 0 {
		log.Info("No pods running, cluster is stopped")
		galera.Status.Bootstrapped = false
		galera.Status.Attributes = make(map[string]databasev1.GaleraAttributes)
		err := r.Status().Update(ctx, galera)
		if err != nil {
			log.Error(err, "Failed to update Galera status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	knownNodes := []string{}
	for k := range galera.Status.Attributes {
		knownNodes = append(knownNodes, k)
	}
	sort.Strings(knownNodes)
	nodesDiffer := !reflect.DeepEqual(podNames, knownNodes)

	removedNodes := []string{}
	for k := range galera.Status.Attributes {
		present := false
		for _, n := range podNames {
			if k == n {
				present = true
				break
			}
		}
		if !present {
			removedNodes = append(removedNodes, k)
		}
	}

	// In case some pods got deleted, clean the associated internal status
	if len(removedNodes) > 0 {
		for _, n := range removedNodes {
			delete(galera.Status.Attributes, n)
		}
		log.Info("Pod removed, cleaning internal status", "pods", removedNodes)
		err = r.Status().Update(ctx, galera)
		if err != nil {
			log.Error(err, "Failed to update Galera status")
			return ctrl.Result{}, err
		}
		// Status updated - return and requeue
		return ctrl.Result{Requeue: true}, nil
	}

	// In case the number of pods doesn't match our known status,
	// scan each pod's database for seqno if not done already
	if nodesDiffer {
		log.Info("New pod config detected, wait for pod availability before probing", "podNames", podNames, "knownNodes", knownNodes)
		if galera.Status.Attributes == nil {
			galera.Status.Attributes = make(map[string]databasev1.GaleraAttributes)
		}
		for _, pod := range podList.Items {
			if _, k := galera.Status.Attributes[pod.Name]; !k {
				if pod.Status.Phase == corev1.PodRunning {
					log.Info("Pod running, retrieve seqno", "pod", pod.Name)
					stdout, _, rc := execInPod(r.config, r.fullClient, galera.Namespace, pod.Name, "galera", []string{"/bin/bash", "/var/lib/operator-scripts/detect_last_commit.sh"})
					if rc != nil {
						log.Error(err, "Failed to retrieve seqno from galera database", "pod", pod.Name)
						return ctrl.Result{}, err
					}
					seqno := strings.TrimSuffix(stdout.String(), "\n")
					attr := databasev1.GaleraAttributes{
						Seqno: seqno,
					}
					galera.Status.Attributes[pod.Name] = attr
					err := r.Status().Update(ctx, galera)
					if err != nil {
						log.Error(err, "Failed to update Galera status")
						return ctrl.Result{}, err
					}
					return ctrl.Result{Requeue: true}, nil
					// // Requeue in case we can handle other pods (TODO: be smarter than 3s)
					// return ctrl.Result{RequeueAfter: time.Duration(3) * time.Second}, nil
				} else {
					// TODO check if another pod can be probed before bailing out
					// This pod hasn't started fully, we can't introspect the galera database yet
					// so we requeue the event for processing later
					return ctrl.Result{RequeueAfter: time.Duration(3) * time.Second}, nil
				}
			}
		}
	}

	bootstrapped := galera.Status.Bootstrapped == true
	if !bootstrapped && len(podNames) == len(galera.Status.Attributes) {
		node := FindBestCandidate(&galera.Status)
		uri := "gcomm://"
		log.Info("Pushing gcomm URI to bootstrap", "pod", node)

		// rc := injectGcommURI(r, galera, node, uri)
		// if rc != nil {
		// 	log.Error(err, "Failed to push gcomm URI", "pod", node)
		// 	return ctrl.Result{}, rc
		// }

		_, _, rc := execInPod(r.config, r.fullClient, galera.Namespace, node, "galera", []string{"/bin/bash", "-c", "echo '" + uri + "' > /tmp/gcomm_uri"})
		if rc != nil {
			log.Error(err, "Failed to push gcomm URI", "pod", node)
			return ctrl.Result{}, err
		}
		attr := galera.Status.Attributes[node]
		attr.Gcomm = uri
		galera.Status.Attributes[node] = attr

		galera.Status.Bootstrapped = true
		err := r.Status().Update(ctx, galera)
		if err != nil {
			log.Error(err, "Failed to update Galera status")
			return ctrl.Result{}, err
		}
		// Requeue in case we can handle other pods (TODO: be smarter than 3s)
		return ctrl.Result{RequeueAfter: time.Duration(3) * time.Second}, nil
	}

	if bootstrapped {
		size := int(galera.Spec.Size)
		for i := 0; i < size; i++ {
			node := galera.Name + "-" + strconv.Itoa(i)
			attr, found := galera.Status.Attributes[node]
			if !found || attr.Gcomm != "" {
				continue
			}

			uri := BuildGcommURI(galera)
			log.Info("Pushing gcomm URI to joiner", "pod", node)
			// rc := injectGcommURI(r, galera, node, uri)
			// if rc != nil {
			// 	log.Error(err, "Failed to push gcomm URI", "pod", node)
			// 	return ctrl.Result{}, rc
			// }

			_, _, rc := execInPod(r.config, r.fullClient, galera.Namespace, node, "galera", []string{"/bin/bash", "-c", "echo '" + uri + "' > /tmp/gcomm_uri"})
			if rc != nil {
				log.Error(err, "Failed to push gcomm URI", "pod", node)
				return ctrl.Result{}, rc
			}
			attr.Gcomm = uri
			galera.Status.Attributes[node] = attr

			err := r.Status().Update(ctx, galera)
			if err != nil {
				log.Error(err, "Failed to update Galera status")
				return ctrl.Result{}, err
			}
			// Requeue in case we can handle other pods (TODO: be smarter than 3s)
			return ctrl.Result{RequeueAfter: time.Duration(3) * time.Second}, nil
		}
	}

	return ctrl.Result{}, nil
}

// statefulSetForGalera returns a galera StatefulSet object
func (r *GaleraReconciler) statefulSetForGalera(m *databasev1.Galera) *appsv1.StatefulSet {
	ls := labelsForGalera(m.Name)
	replicas := m.Spec.Size
	runAsUser := int64(0)
	// storage := "local-storage"
	storage := "host-nfs-storageclass"
	dep := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "galera",
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "galera",
					InitContainers: []corev1.Container{{
						Image:   m.Spec.Image,
						Name:    "mysql-bootstrap",
						Command: []string{"bash", "/var/lib/operator-scripts/mysql_bootstrap.sh"},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser: &runAsUser,
						},
						Env: []corev1.EnvVar{{
							Name:  "KOLLA_BOOTSTRAP",
							Value: "True",
						}, {
							Name:  "KOLLA_CONFIG_STRATEGY",
							Value: "COPY_ALWAYS",
						}, {
							Name: "DB_ROOT_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: m.Spec.Secret,
									},
									Key: "root_password",
								},
							},
						}},
						VolumeMounts: []corev1.VolumeMount{{
							MountPath: "/var/lib/mysql",
							Name:      "mysql-db",
						}, {
							MountPath: "/var/lib/config-data",
							ReadOnly:  true,
							Name:      "config-data",
						}, {
							MountPath: "/var/lib/pod-config-data",
							Name:      "pod-config-data",
						}, {
							MountPath: "/var/lib/operator-scripts",
							ReadOnly:  true,
							Name:      "operator-scripts",
						}, {
							MountPath: "/var/lib/kolla/config_files",
							ReadOnly:  true,
							Name:      "kolla-config",
						}},
					}},
					Containers: []corev1.Container{{
						Image: m.Spec.Image,
						// ImagePullPolicy: "Always",
						Name:    "galera",
						Command: []string{"kolla_start"},
						Env: []corev1.EnvVar{{
							Name:  "KOLLA_CONFIG_STRATEGY",
							Value: "COPY_ALWAYS",
						}, {
							Name: "DB_ROOT_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: m.Spec.Secret,
									},
									Key: "root_password",
								},
							},
						}},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser: &runAsUser,
						},
						Ports: []corev1.ContainerPort{{
							ContainerPort: 3306,
							Name:          "mysql",
						}, {
							ContainerPort: 4567,
							Name:          "galera",
						}},
						VolumeMounts: []corev1.VolumeMount{{
							MountPath: "/var/lib/mysql",
							Name:      "mysql-db",
						}, {
							MountPath: "/var/lib/config-data",
							ReadOnly:  true,
							Name:      "config-data",
						}, {
							MountPath: "/var/lib/pod-config-data",
							Name:      "pod-config-data",
						}, {
							MountPath: "/var/lib/operator-scripts",
							ReadOnly:  true,
							Name:      "operator-scripts",
						}, {
							MountPath: "/var/lib/kolla/config_files",
							ReadOnly:  true,
							Name:      "kolla-config",
						}},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "kolla-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "galera-config",
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "config.json",
											Path: "config.json",
										},
									},
								},
							},
						},
						{
							Name: "pod-config-data",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "config-data",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "galera-config",
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "galera.cnf.in",
											Path: "galera.cnf.in",
										},
										{
											Key:  "galera.cnf",
											Path: "galera.cnf",
										},
									},
								},
							},
						},
						{
							Name: "operator-scripts",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "galera-config",
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "mysql_bootstrap.sh",
											Path: "mysql_bootstrap.sh",
										},
										{
											Key:  "detect_last_commit.sh",
											Path: "detect_last_commit.sh",
										},
										{
											Key:  "detect_gcomm_and_start.sh",
											Path: "detect_gcomm_and_start.sh",
										},
									},
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "mysql-db",
						Labels: ls,
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							"ReadWriteOnce",
						},
						StorageClassName: &storage,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								"storage": resource.MustParse("3Gi"),
							},
						},
					},
				},
			},
		},
	}
	// Set Galera instance as the owner and controller
	ctrl.SetControllerReference(m, dep, r.Scheme)
	return dep
}

// statefulSetForGalera returns a galera StatefulSet object
func (r *GaleraReconciler) headlessServiceForGalera(m *databasev1.Galera) *corev1.Service {
	// ls := labelsForGalera(m.Name)
	dep := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:      "ClusterIP",
			ClusterIP: "None",
			Ports: []corev1.ServicePort{
				{Name: "mysql", Protocol: "TCP", Port: 3306},
			},
			Selector: map[string]string{
				"app": "galera",
			},
		},
	}
	// Set Galera instance as the owner and controller
	ctrl.SetControllerReference(m, dep, r.Scheme)
	return dep
}

// labelsForGalera returns the labels for selecting the resources
// belonging to the given galera CR name.
func labelsForGalera(name string) map[string]string {
	return map[string]string{"app": "galera", "galera_cr": name}
}

// getPodNames returns the pod names of the array of pods passed in
func getPodNames(pods []corev1.Pod) []string {
	var podNames []string
	for _, pod := range pods {
		podNames = append(podNames, pod.Name)
	}
	sort.Strings(podNames)
	return podNames
}

// SetupWithManager sets up the controller with the Manager.
func (r *GaleraReconciler) SetupWithManager(mgr ctrl.Manager) error {
	var err error
	r.config = mgr.GetConfig()
	if r.configClient, err = configclient.NewForConfig(r.config); err != nil {
		return err
	}

	if r.fullClient, err = kubernetes.NewForConfig(r.config); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.Galera{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)
}
