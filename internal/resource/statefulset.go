package resource

import (
	"fmt"

	"github.com/pivotal/rabbitmq-for-kubernetes/internal/metadata"
	k8sresource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	rabbitmqv1beta1 "github.com/pivotal/rabbitmq-for-kubernetes/api/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultGracePeriodTimeoutSeconds int64  = 60 * 60 * 24 * 7
	initContainerCPU                 string = "100m"
	initContainerMemory              string = "500Mi"
)

func (builder *RabbitmqResourceBuilder) StatefulSet() *StatefulSetBuilder {
	return &StatefulSetBuilder{
		Instance: builder.Instance,
		Scheme:   builder.Scheme,
	}
}

type StatefulSetBuilder struct {
	Instance *rabbitmqv1beta1.RabbitmqCluster
	Scheme   *runtime.Scheme
}

func (builder *StatefulSetBuilder) Build() (runtime.Object, error) {
	return builder.statefulSet()
}

func (builder *StatefulSetBuilder) statefulSet() (*appsv1.StatefulSet, error) {
	// PVC, ServiceName & Selector: can't be updated without deleting the statefulset
	pvc, err := persistentVolumeClaim(builder.Instance, builder.Scheme)
	if err != nil {
		return nil, err
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      builder.Instance.ChildResourceName("server"),
			Namespace: builder.Instance.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: builder.Instance.ChildResourceName(headlessServiceName),
			Selector: &metav1.LabelSelector{
				MatchLabels: metadata.LabelSelector(builder.Instance.Name),
			},
			VolumeClaimTemplates: pvc,
		},
	}, nil
}

func (builder *StatefulSetBuilder) Update(object runtime.Object) error {
	sts := object.(*appsv1.StatefulSet)
	podAnnotations := metadata.ReconcileAnnotations(sts.Spec.Template.Annotations, builder.Instance.Annotations)
	annotations := metadata.ReconcileAnnotations(sts.Annotations, builder.Instance.Annotations)
	sts.Spec.Template = builder.podTemplateSpec()
	sts.Spec.Template.ObjectMeta.Annotations = podAnnotations
	sts.Annotations = annotations
	return builder.setStatefulSetParams(sts)
}

func (builder *StatefulSetBuilder) setStatefulSetParams(sts *appsv1.StatefulSet) error {

	if err := controllerutil.SetControllerReference(builder.Instance, sts, builder.Scheme); err != nil {
		return fmt.Errorf("failed setting controller reference: %v", err)
	}

	//Labels
	updatedLabels := metadata.GetLabels(builder.Instance.Name, builder.Instance.Labels)
	sts.Labels = updatedLabels
	sts.Spec.Template.ObjectMeta.Labels = updatedLabels

	//Spec

	//Replicas
	replicas := builder.Instance.Spec.Replicas
	sts.Spec.Replicas = &replicas

	//Init Container resources
	cpuRequest, err := k8sresource.ParseQuantity(initContainerCPU)
	if err != nil {
		return err
	}
	memoryRequest, err := k8sresource.ParseQuantity(initContainerMemory)
	if err != nil {
		return err
	}

	//Template

	//Container resources
	sts.Spec.Template.Spec.InitContainers[0].Resources = corev1.ResourceRequirements{
		Limits: map[corev1.ResourceName]k8sresource.Quantity{
			"cpu":    cpuRequest,
			"memory": memoryRequest,
		},
		Requests: map[corev1.ResourceName]k8sresource.Quantity{
			"cpu":    cpuRequest,
			"memory": memoryRequest,
		},
	}

	sts.Spec.Template.Spec.Containers[0].Resources = *builder.Instance.Spec.Resources
	if !sts.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().Equal(*sts.Spec.Template.Spec.Containers[0].Resources.Requests.Memory()) {
		logger := ctrl.Log.WithName("statefulset").WithName("RabbitmqCluster")
		logger.Info(fmt.Sprintf("Warning: Memory request and limit are not equal for \"%s\". It is recommended that they be set to the same value", sts.GetName()))
	}

	// Container images
	sts.Spec.Template.Spec.InitContainers[0].Image = builder.Instance.Spec.Image
	sts.Spec.Template.Spec.Containers[0].Image = builder.Instance.Spec.Image

	//Image Pull Secret
	sts.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{}
	if builder.Instance.Spec.ImagePullSecret != "" {
		sts.Spec.Template.Spec.ImagePullSecrets = append(sts.Spec.Template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: builder.Instance.Spec.ImagePullSecret})
	}

	//Affinity & Tolerations
	sts.Spec.Template.Spec.Affinity = builder.Instance.Spec.Affinity
	sts.Spec.Template.Spec.Tolerations = builder.Instance.Spec.Tolerations

	return nil
}

func persistentVolumeClaim(instance *rabbitmqv1beta1.RabbitmqCluster, scheme *runtime.Scheme) ([]corev1.PersistentVolumeClaim, error) {
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "persistence",
			Namespace:   instance.GetNamespace(),
			Labels:      metadata.Label(instance.Name),
			Annotations: metadata.ReconcileAnnotations(map[string]string{}, instance.Annotations),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *instance.Spec.Persistence.Storage,
				},
			},
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: instance.Spec.Persistence.StorageClassName,
		},
	}

	if err := controllerutil.SetControllerReference(instance, &pvc, scheme); err != nil {
		return []corev1.PersistentVolumeClaim{}, fmt.Errorf("failed setting controller reference: %v", err)
	}

	return []corev1.PersistentVolumeClaim{pvc}, nil
}

func (builder *StatefulSetBuilder) podTemplateSpec() corev1.PodTemplateSpec {
	automountServiceAccountToken := true
	rabbitmqGID := int64(999)
	rabbitmqUID := int64(999)

	terminationGracePeriod := defaultGracePeriodTimeoutSeconds
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				FSGroup:    &rabbitmqGID,
				RunAsGroup: &rabbitmqGID,
				RunAsUser:  &rabbitmqUID,
			},
			TerminationGracePeriodSeconds: &terminationGracePeriod,
			ServiceAccountName:            builder.Instance.ChildResourceName(serviceAccountName),
			AutomountServiceAccountToken:  &automountServiceAccountToken,
			InitContainers: []corev1.Container{
				{
					Name: "copy-config",
					Command: []string{
						"sh", "-c", "cp /tmp/rabbitmq/rabbitmq.conf /etc/rabbitmq/rabbitmq.conf && echo '' >> /etc/rabbitmq/rabbitmq.conf ; " +
							"cp /tmp/erlang-cookie-secret/.erlang.cookie /var/lib/rabbitmq/.erlang.cookie " +
							"&& chown 999:999 /var/lib/rabbitmq/.erlang.cookie " +
							"&& chmod 600 /var/lib/rabbitmq/.erlang.cookie",
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "server-conf",
							MountPath: "/tmp/rabbitmq/",
						},
						{
							Name:      "rabbitmq-etc",
							MountPath: "/etc/rabbitmq/",
						},
						{
							Name:      "rabbitmq-erlang-cookie",
							MountPath: "/var/lib/rabbitmq/",
						},
						{
							Name:      "erlang-cookie-secret",
							MountPath: "/tmp/erlang-cookie-secret/",
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name: "rabbitmq",
					Env: []corev1.EnvVar{
						{
							Name:  "RABBITMQ_ENABLED_PLUGINS_FILE",
							Value: "/opt/server-conf/enabled_plugins",
						},
						{
							Name:  "RABBITMQ_DEFAULT_PASS_FILE",
							Value: "/opt/rabbitmq-secret/password",
						},
						{
							Name:  "RABBITMQ_DEFAULT_USER_FILE",
							Value: "/opt/rabbitmq-secret/username",
						},
						{
							Name:  "RABBITMQ_MNESIA_BASE",
							Value: "/var/lib/rabbitmq/db",
						},
						{
							Name: "MY_POD_NAME",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{
									FieldPath:  "metadata.name",
									APIVersion: "v1",
								},
							},
						},
						{
							Name: "MY_POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{
									FieldPath:  "metadata.namespace",
									APIVersion: "v1",
								},
							},
						},
						{
							Name:  "K8S_SERVICE_NAME",
							Value: builder.Instance.ChildResourceName("headless"),
						},
						{
							Name:  "RABBITMQ_USE_LONGNAME",
							Value: "true",
						},
						{
							Name:  "RABBITMQ_NODENAME",
							Value: "rabbit@$(MY_POD_NAME).$(K8S_SERVICE_NAME).$(MY_POD_NAMESPACE).svc.cluster.local",
						},
						{
							Name:  "K8S_HOSTNAME_SUFFIX",
							Value: ".$(K8S_SERVICE_NAME).$(MY_POD_NAMESPACE).svc.cluster.local",
						},
					},
					Ports: []corev1.ContainerPort{
						{
							Name:          "epmd",
							ContainerPort: 4369,
						},
						{
							Name:          "amqp",
							ContainerPort: 5672,
						},
						{
							Name:          "http",
							ContainerPort: 15672,
						},
						{
							Name:          "prometheus",
							ContainerPort: 15692,
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "server-conf",
							MountPath: "/opt/server-conf/",
						},
						{
							Name:      "rabbitmq-admin",
							MountPath: "/opt/rabbitmq-secret/",
						},
						{
							Name:      "persistence",
							MountPath: "/var/lib/rabbitmq/db/",
						},
						{
							Name:      "rabbitmq-etc",
							MountPath: "/etc/rabbitmq/",
						},
						{
							Name:      "rabbitmq-erlang-cookie",
							MountPath: "/var/lib/rabbitmq/",
						},
					},
					ReadinessProbe: &corev1.Probe{
						Handler: corev1.Handler{
							Exec: &corev1.ExecAction{
								Command: []string{"/bin/sh", "-c", "rabbitmq-diagnostics check_port_connectivity"},
							},
						},
						InitialDelaySeconds: 10,
						TimeoutSeconds:      5,
						PeriodSeconds:       30,
						SuccessThreshold:    1,
						FailureThreshold:    3,
					},
					Lifecycle: &corev1.Lifecycle{
						PreStop: &corev1.Handler{
							Exec: &corev1.ExecAction{
								Command: []string{
									"/bin/bash", "-c", "while true; do rabbitmq-queues check_if_node_is_quorum_critical" +
										" 2>&1; if [ $(echo $?) -eq 69 ]; then sleep 2; continue; fi;" +
										" rabbitmq-queues check_if_node_is_mirror_sync_critical" +
										" 2>&1; if [ $(echo $?) -eq 69 ]; then sleep 2; continue; fi; break;" +
										" done",
								},
							},
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "rabbitmq-admin",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: builder.Instance.ChildResourceName(adminSecretName),
							Items: []corev1.KeyToPath{
								{
									Key:  "username",
									Path: "username",
								},
								{
									Key:  "password",
									Path: "password",
								},
							},
						},
					},
				},
				{
					Name: "server-conf",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: builder.Instance.ChildResourceName(serverConfigMapName),
							},
						},
					},
				},
				{
					Name: "rabbitmq-etc",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "rabbitmq-erlang-cookie",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
				{
					Name: "erlang-cookie-secret",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: builder.Instance.ChildResourceName(erlangCookieName),
						},
					},
				},
			},
		},
	}
}
