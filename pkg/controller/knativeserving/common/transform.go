package common

import (
	"os"

	mf "github.com/jcrossley3/manifestival"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func replaceImageFromEnvironment(prefix string, scheme *runtime.Scheme) mf.Transformer {
	return func(u *unstructured.Unstructured) error {
		if u.GetKind() == "Deployment" {
			image := os.Getenv(prefix + u.GetName())
			if len(image) > 0 {
				deploy := &appsv1.Deployment{}
				if err := scheme.Convert(u, deploy, nil); err != nil {
					return err
				}
				containers := deploy.Spec.Template.Spec.Containers
				for i, container := range containers {
					if container.Name == u.GetName() && container.Image != image {
						log.Info("Replacing", "deployment", container.Name, "image", image)
						containers[i].Image = image
						break
					}
				}
				if err := scheme.Convert(deploy, u, nil); err != nil {
					return err
				}
			}
		}
		return nil
	}
}
