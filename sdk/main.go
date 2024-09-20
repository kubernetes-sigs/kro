package main

import (
	"github.com/aws-controllers-k8s/private-symphony/cli/symphony"
	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DeploymentService struct {
	metav1.TypeMeta
	metav1.ObjectMeta
	Spec struct {
		Replicas int `default:"1"`
		Port     int `default:"80"`
		Name     string
	} `json:"spec"`
	Status struct {
		AvailableReplicas int                `ref:"deployment.status.availableReplicas"`
		Conditions        []metav1.Condition `ref:"deployment.status.conditions"`
	} `json:"status"`
}

func main() {
	symphony.Compile(v1alpha1.ResourceGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: "MyResourceGroup",
		},
		Spec: v1alpha1.ResourceGroupSpec{
			Kind:       "example.com",
			APIVersion: "v1",
			Definition: symphony.Definition[DeploymentService](),
			Resources: []*v1alpha1.Resource{
				symphony.Resource("deployment", &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: symphony.Ref[string]("spec.name"),
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: lo.ToPtr[int32](1),
						Selector: &metav1.LabelSelector{MatchLabels: symphony.Ref[map[string]string]("metadata.labels")},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: symphony.Ref[map[string]string]("metadata.labels"),
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  symphony.Ref[string]("metadata.name"),
									Image: "nginx",
									Ports: []corev1.ContainerPort{{
										ContainerPort: symphony.Ref[int32]("spec.port"),
									}},
								}},
							},
						},
					},
				}),
				symphony.Resource("service", &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name: symphony.Ref[string]("metadata.name"),
					},
					Spec: corev1.ServiceSpec{
						Selector: symphony.Ref[map[string]string]("metadata.labels"),
						Ports: []corev1.ServicePort{{
							Port: symphony.Ref[int32]("spec.port"),
						}},
					},
				}),
			},
		},
	})
}
