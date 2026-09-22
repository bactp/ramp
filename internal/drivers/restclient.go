package drivers

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// withCoreV1Defaults fills in the group/version/codec fields a bare
// rest.Config lacks, so it can be used to build a core/v1 REST client for the
// node proxy subresource.
func withCoreV1Defaults(cfg *rest.Config) *rest.Config {
	gv := corev1.SchemeGroupVersion
	cfg.GroupVersion = &gv
	cfg.APIPath = "/api"
	cfg.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: scheme.Codecs}
	if cfg.UserAgent == "" {
		cfg.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	_ = runtime.NewScheme
	return cfg
}
