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
	configmap "github.com/openstack-k8s-operators/lib-common/modules/common/configmap"
	env "github.com/openstack-k8s-operators/lib-common/modules/common/env"
	helper "github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	util "github.com/openstack-k8s-operators/lib-common/modules/common/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"bytes"
	"context"
	"fmt"
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

	databasev1beta1 "github.com/dciabrin/galera-operator/api/v1beta1"
)

// GaleraReconciler reconciles a Galera object
type GaleraReconciler struct {
	client.Client
	Kclient kubernetes.Interface
	config  *rest.Config
	Log     logr.Logger
	Scheme  *runtime.Scheme
}

func execInPod(r *GaleraReconciler, namespace string, pod string, container string, cmd []string, fun func(*bytes.Buffer, *bytes.Buffer) error) error {
	req := r.Kclient.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").Param("container", container)
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

	exec, err := remotecommand.NewSPDYExecutor(r.config, "POST", req.URL())
	if err != nil {
		return err
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})

	if err != nil {
		return err
	} else {
		return fun(&stdout, &stderr)
	}
}

func FindBestCandidate(status *databasev1beta1.GaleraStatus) string {
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

func BuildGcommURI(galera *databasev1beta1.Galera) string {
	size := int(galera.Spec.Size)
	basename := galera.Name + "-galera"
	res := []string{}

	for i := 0; i < size; i++ {
		res = append(res, basename+"-"+strconv.Itoa(i)+"."+basename)
	}
	uri := "gcomm://" + strings.Join(res, ",")
	return uri // gcomm://galera-0.galera,galera-1.galera,galera-2.galera
}

// generate rbac to get, list, watch, create, update and patch the galeras status the galera resource
// +kubebuilder:rbac:groups=mariadb.openstack.org,resources=galeras,verbs=get;list;watch;create;update;patch;delete

// generate rbac to get, update and patch the galera status the galera/finalizers
// +kubebuilder:rbac:groups=mariadb.openstack.org,resources=galeras/status,verbs=get;update;patch

// generate rbac to update the galera/finalizers
// +kubebuilder:rbac:groups=mariadb.openstack.org,resources=galeras/finalizers,verbs=update

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
	resName := resourceNameForGalera(req.NamespacedName.Name)

	// Fetch the Galera instance
	galera := &databasev1beta1.Galera{}
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

	helper, err := helper.NewHelper(
		galera,
		r.Client,
		r.Kclient,
		r.Scheme,
		r.Log,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check if the mariadb shim object already exists, if not create a new one
	mdbfound := &databasev1beta1.MariaDB{}
	mdbname := types.NamespacedName{Namespace: req.NamespacedName.Namespace, Name: req.NamespacedName.Name}
	err = r.Get(ctx, mdbname, mdbfound)
	if err != nil && errors.IsNotFound(err) {
		// Define a new MariaDB object
		mdb := r.mariadbShimForGalera(galera)
		log.Info("Creating a new MariaDB", "MariaDB.Namespace", mdbname.Namespace, "MariaDB.Name", mdbname.Name)
		err = r.Create(ctx, mdb)
		if err != nil {
			log.Error(err, "Failed to create new MariaDB", "MariaDB.Namespace", mdbname.Namespace, "MariaDB.Name", mdbname.Name)
			return ctrl.Result{}, err
		}
		// StatefulSet created successfully - return and requeue
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Error(err, "Failed to get MariaDB")
		return ctrl.Result{}, err
	}

	// Check if the StatefulSet already exists, if not create a new one
	found := &appsv1.StatefulSet{}
	ssetname := types.NamespacedName{Namespace: req.NamespacedName.Namespace, Name: resName}
	err = r.Get(ctx, ssetname, found)
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
	hsvcfound := &corev1.Service{}
	hsvcname := types.NamespacedName{Namespace: req.NamespacedName.Namespace, Name: resName}
	err = r.Get(ctx, hsvcname, hsvcfound)
	if err != nil && errors.IsNotFound(err) {
		// Define a new headless service
		hsvc := r.headlessServiceForGalera(galera)
		log.Info("Creating a new headless Service", "StatefulSet.Namespace", hsvc.Namespace, "StatefulSet.Name", hsvc.Name)
		err = r.Create(ctx, hsvc)
		if err != nil {
			log.Error(err, "Failed to create new headless Service", "StatefulSet.Namespace", hsvc.Namespace, "StatefulSet.Name", hsvc.Name)
			return ctrl.Result{}, err
		}
		// headless service created successfully - return and requeue
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Error(err, "Failed to get headless Service")
		return ctrl.Result{}, err
	}

	// Check if the main mysql service already exists, if not create a new one
	svcfound := &corev1.Service{}
	nsname := types.NamespacedName{Namespace: req.NamespacedName.Namespace, Name: "openstack"}
	err = r.Get(ctx, nsname, svcfound)
	if err != nil && errors.IsNotFound(err) {
		// Define a new headless service
		svc := r.serviceForGalera(galera)
		log.Info("Creating a new Service", "StatefulSet.Namespace", svc.Namespace, "StatefulSet.Name", svc.Name)
		err = r.Create(ctx, svc)
		if err != nil {
			log.Error(err, "Failed to create main Service", "StatefulSet.Namespace", svc.Namespace, "StatefulSet.Name", svc.Name)
			return ctrl.Result{}, err
		}
		// service created successfully - return and requeue
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Service")
		return ctrl.Result{}, err
	}

	customData := make(map[string]string)
	templateParameters := make(map[string]interface{})

	cms := []util.Template{
		// ScriptsConfigMap
		{
			Name:         fmt.Sprintf("%s-scripts", galera.Name),
			Namespace:    galera.Namespace,
			Type:         util.TemplateTypeScripts,
			InstanceType: galera.Kind,
			AdditionalTemplate: map[string]string{
				"mysql_bootstrap.sh":        "/galera/bin/mysql_bootstrap.sh",
				"mysql_probe.sh":            "/galera/bin/mysql_probe.sh",
				"detect_last_commit.sh":     "/galera/bin/detect_last_commit.sh",
				"detect_gcomm_and_start.sh": "/galera/bin/detect_gcomm_and_start.sh",
			},
			Labels: map[string]string{},
		},
		// ConfigMap
		{
			Name:          fmt.Sprintf("%s-config-data", galera.Name),
			Namespace:     galera.Namespace,
			Type:          util.TemplateTypeConfig,
			InstanceType:  galera.Kind,
			CustomData:    customData,
			ConfigOptions: templateParameters,
			Labels:        map[string]string{},
		},
	}

	envVars := make(map[string]env.Setter)

	errmap := configmap.EnsureConfigMaps(ctx, helper, galera, cms, &envVars)
	if errmap != nil {
		log.Error(errmap, "Unable to retrieve or create config maps")
		return ctrl.Result{}, errmap
	}

	// log.Info("*******", "envVars", envVars)

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
		galera.Status.Attributes = make(map[string]databasev1beta1.GaleraAttributes)
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
			galera.Status.Attributes = make(map[string]databasev1beta1.GaleraAttributes)
		}
		for _, pod := range podList.Items {
			if _, k := galera.Status.Attributes[pod.Name]; !k {
				if pod.Status.Phase == corev1.PodRunning {
					log.Info("Pod running, retrieve seqno", "pod", pod.Name)
					rc := execInPod(r, galera.Namespace, pod.Name, "galera",
						[]string{"/bin/bash", "/var/lib/operator-scripts/detect_last_commit.sh"},
						func(stdout *bytes.Buffer, stderr *bytes.Buffer) error {
							seqno := strings.TrimSuffix(stdout.String(), "\n")
							attr := databasev1beta1.GaleraAttributes{
								Seqno: seqno,
							}
							galera.Status.Attributes[pod.Name] = attr
							return nil
						})
					if rc != nil {
						log.Error(err, "Failed to retrieve seqno from galera database", "pod", pod.Name, "rc", rc)
						return ctrl.Result{}, err
					}
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

	// log.Info("db", "status", galera.Status)
	bootstrapped := galera.Status.Bootstrapped == true
	if !bootstrapped && len(podNames) == len(galera.Status.Attributes) {
		node := FindBestCandidate(&galera.Status)
		uri := "gcomm://"
		log.Info("Pushing gcomm URI to bootstrap", "pod", node)

		rc := execInPod(r, galera.Namespace, node, "galera",
			[]string{"/bin/bash", "-c", "echo '" + uri + "' > /tmp/gcomm_uri"},
			func(stdout *bytes.Buffer, stderr *bytes.Buffer) error {
				attr := galera.Status.Attributes[node]
				attr.Gcomm = uri
				galera.Status.Attributes[node] = attr
				galera.Status.Bootstrapped = true
				return nil
			})
		if rc != nil {
			log.Error(err, "Failed to push gcomm URI", "pod", node, "rc", rc)
			return ctrl.Result{}, rc
		}
		err := r.Status().Update(ctx, galera)
		if err != nil {
			log.Error(err, "Failed to update Galera status")
			return ctrl.Result{}, err
		}
		// Requeue in case we can handle other pods (TODO: be smarter than 3s)
		return ctrl.Result{RequeueAfter: time.Duration(5) * time.Second}, nil
	}

	if bootstrapped {
		size := int(galera.Spec.Size)
		baseName := resourceNameForGalera(galera.Name)
		for i := 0; i < size; i++ {
			node := baseName + "-" + strconv.Itoa(i)
			attr, found := galera.Status.Attributes[node]
			if !found || attr.Gcomm != "" {
				continue
			}

			uri := BuildGcommURI(galera)
			log.Info("Pushing gcomm URI to joiner", "pod", node)

			rc := execInPod(r, galera.Namespace, node, "galera",
				[]string{"/bin/bash", "-c", "echo '" + uri + "' > /tmp/gcomm_uri"},
				func(stdout *bytes.Buffer, stderr *bytes.Buffer) error {
					attr.Gcomm = uri
					galera.Status.Attributes[node] = attr
					return nil
				})
			if rc != nil {
				log.Error(err, "Failed to push gcomm URI", "pod", node, "rc", rc)
				return ctrl.Result{}, rc
			}
			err := r.Status().Update(ctx, galera)
			if err != nil {
				log.Error(err, "Failed to update Galera status")
				return ctrl.Result{}, err
			}
			// Requeue in case we can handle other pods (TODO: be smarter than 3s)
			// log.Info("Requeue", "pod", node)
			return ctrl.Result{RequeueAfter: time.Duration(5) * time.Second}, nil
		}
	}

	return ctrl.Result{}, nil
}

// statefulSetForGalera returns a galera StatefulSet object
func (r *GaleraReconciler) statefulSetForGalera(m *databasev1beta1.Galera) *appsv1.StatefulSet {
	ls := labelsForGalera(m.Name)
	name := resourceNameForGalera(m.Name)
	replicas := m.Spec.Size
	runAsUser := int64(0)
	storage := m.Spec.StorageClass
	storageRequest := resource.MustParse(m.Spec.StorageRequest)
	dep := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "galera-operator-galera",
					InitContainers: []corev1.Container{{
						Image:   m.Spec.ContainerImage,
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
									Key: "DbRootPassword",
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
						Image: m.Spec.ContainerImage,
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
									Key: "DbRootPassword",
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
							MountPath: "/var/lib/secrets",
							ReadOnly:  true,
							Name:      "secrets",
						}, {
							MountPath: "/var/lib/operator-scripts",
							ReadOnly:  true,
							Name:      "operator-scripts",
						}, {
							MountPath: "/var/lib/kolla/config_files",
							ReadOnly:  true,
							Name:      "kolla-config",
						}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"/bin/bash", "/var/lib/operator-scripts/mysql_probe.sh", "liveness"},
								},
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"/bin/bash", "/var/lib/operator-scripts/mysql_probe.sh", "readiness"},
								},
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "secrets",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: m.Spec.Secret,
									Items: []corev1.KeyToPath{
										{
											Key:  "DbRootPassword",
											Path: "dbpassword",
										},
									},
								},
							},
						},
						{
							Name: "kolla-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: m.Name + "-config-data",
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
										Name: m.Name + "-config-data",
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "galera.cnf.in",
											Path: "galera.cnf.in",
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
										Name: m.Name + "-scripts",
									},
									Items: []corev1.KeyToPath{
										{
											Key:  "mysql_bootstrap.sh",
											Path: "mysql_bootstrap.sh",
										},
										{
											Key:  "mysql_probe.sh",
											Path: "mysql_probe.sh",
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
								"storage": storageRequest,
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

func (r *GaleraReconciler) mariadbShimForGalera(m *databasev1beta1.Galera) *databasev1beta1.MariaDB {
	// ls := labelsForGalera(m.Name)
	mariadb := &databasev1beta1.MariaDB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
		},
		Spec: databasev1beta1.MariaDBSpec{
			Secret:         m.Spec.Secret,
			StorageClass:   m.Spec.StorageClass,
			StorageRequest: m.Spec.StorageRequest,
			ContainerImage: m.Spec.ContainerImage,
		},
	}
	ctrl.SetControllerReference(m, mariadb, r.Scheme)
	return mariadb
}

func (r *GaleraReconciler) headlessServiceForGalera(m *databasev1beta1.Galera) *corev1.Service {
	// ls := labelsForGalera(m.Name)
	name := resourceNameForGalera(m.Name)
	dep := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:      "ClusterIP",
			ClusterIP: "None",
			Ports: []corev1.ServicePort{
				{Name: "mysql", Protocol: "TCP", Port: 3306},
			},
			Selector: map[string]string{
				"app": name,
			},
		},
	}
	// Set Galera instance as the owner and controller
	ctrl.SetControllerReference(m, dep, r.Scheme)
	return dep
}

func (r *GaleraReconciler) serviceForGalera(m *databasev1beta1.Galera) *corev1.Service {
	// ls := labelsForGalera(m.Name)
	res := resourceNameForGalera(m.Name)
	dep := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"app": "mariadb",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: "ClusterIP",
			Ports: []corev1.ServicePort{
				{Name: "mysql", Protocol: "TCP", Port: 3306},
			},
			Selector: map[string]string{
				"app": res,
			},
		},
	}
	// Set Galera instance as the owner and controller
	ctrl.SetControllerReference(m, dep, r.Scheme)
	return dep
}

// labelsForGalera returns the labels for selecting the resources
// belonging to the given galera CR name.
func resourceNameForGalera(name string) string {
	return name + "-galera"
}

// labelsForGalera returns the labels for selecting the resources
// belonging to the given galera CR name.
func labelsForGalera(name string) map[string]string {
	return map[string]string{"app": resourceNameForGalera(name), "galera_cr": name}
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
	r.config = mgr.GetConfig()
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1beta1.Galera{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)
}
