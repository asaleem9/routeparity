// Package convert turns Ingress manifests into Gateway API manifests, for runs
// where the user has not written the Gateway side yet.
package convert

import (
	"context"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/asaleem9/routeparity/internal/model"
)

// Options controls a conversion.
type Options struct {
	// IngressClass selects which Ingresses are converted. Empty means "nginx".
	IngressClass string
	// Emitter names the output flavour, for example "standard" or
	// "envoy-gateway". Empty means "standard".
	Emitter string
	// AllowExperimental lets the output use Experimental-channel fields.
	AllowExperimental bool
}

// Output is the result of a conversion.
type Output struct {
	Gateways   []gatewayv1.Gateway
	HTTPRoutes []gatewayv1.HTTPRoute
	// Other holds everything else the converter emitted, such as GRPCRoutes,
	// policies and implementation-specific extensions.
	Other []unstructured.Unstructured
	// Notices are the converter's own messages about what it could not
	// translate. They feed the run's unverified list.
	Notices []Notice
	// Converter identifies the tool and version that produced the output, so
	// that a report can say exactly what was tested.
	Converter string
}

// Notice is one message from the converter.
type Notice struct {
	// Level is "INFO", "WARNING" or "ERROR" as reported by the converter.
	Level   string
	Source  string
	Message string
	// Objects are the objects the message refers to.
	Objects []model.ObjectRef
}

// Converter converts Ingress manifests to Gateway API.
type Converter interface {
	Convert(ctx context.Context, manifests io.Reader, opts Options) (*Output, error)
}
