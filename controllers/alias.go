/*
Copyright 2023 The Kubernetes Authors.

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
	"k8s.io/client-go/rest"
	providercontroller "sigs.k8s.io/cluster-api-operator/internal/controller"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

type GenericProviderReconciler struct {
	Provider     client.Object
	ProviderList client.ObjectList
	Client       client.Client
	Config       *rest.Config
}

func (r *GenericProviderReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	return (&providercontroller.GenericProviderReconciler{
		Provider:     r.Provider,
		ProviderList: r.ProviderList,
		Client:       r.Client,
		Config:       r.Config,
	}).SetupWithManager(mgr, options)
}
