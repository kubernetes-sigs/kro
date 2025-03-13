package pkg

//go:generate go tool controller-gen rbac:roleName="kro:controller:static" crd paths="../..." output:crd:artifacts:config=../helm/crds output:rbac:artifacts:config=../helm/templates
//go:generate ../helm/helmify.sh
