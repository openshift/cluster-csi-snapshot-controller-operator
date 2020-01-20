package common

import (
	"math/rand"
	"time"

	apiextinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
)

const (
	minResyncPeriod = 20 * time.Minute
)

func resyncPeriod() func() time.Duration {
	return func() time.Duration {
		factor := rand.Float64() + 1
		return time.Duration(float64(minResyncPeriod.Nanoseconds()) * factor)
	}
}

// ControllerContext stores all the informers for a variety of kubernetes objects.
type ControllerContext struct {
	ClientBuilder         *Builder
	APIExtInformerFactory apiextinformers.SharedInformerFactory

	Stop <-chan struct{}

	InformersStarted chan struct{}

	ResyncPeriod func() time.Duration
}

// CreateControllerContext creates the ControllerContext with the ClientBuilder.
func CreateControllerContext(cb *Builder, stop <-chan struct{}, targetNamespace string) *ControllerContext {
	apiExtClient := cb.APIExtClientOrDie("apiext-shared-informer")
	apiExtSharedInformer := apiextinformers.NewSharedInformerFactoryWithOptions(apiExtClient, resyncPeriod()(),
		apiextinformers.WithNamespace(targetNamespace))

	return &ControllerContext{
		ClientBuilder:         cb,
		APIExtInformerFactory: apiExtSharedInformer,
		Stop:                  stop,
		InformersStarted:      make(chan struct{}),
		ResyncPeriod:          resyncPeriod(),
	}
}
