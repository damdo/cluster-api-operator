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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	operatorv1 "sigs.k8s.io/cluster-api-operator/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	configMapVersionLabel = "provider.cluster.x-k8s.io/version"
	configMapTypeLabel    = "provider.cluster.x-k8s.io/type"
	configMapNameLabel    = "provider.cluster.x-k8s.io/name"
	operatorManagedLabel  = "managed-by.operator.cluster.x-k8s.io"

	metadataConfigMapKey   = "metadata"
	componentsConfigMapKey = "components"
)

// downloadManifests downloads CAPI manifests from a url.
func (p *phaseReconciler) downloadManifests(ctx context.Context) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	log.Info("Downloading provider manifests")

	// Return immediately if a custom config map is used instead of a url.
	if p.provider.GetSpec().FetchConfig != nil && p.provider.GetSpec().FetchConfig.Selector != nil {
		log.V(5).Info("Custom config map is used, skip downloading provider manifests")

		return reconcile.Result{}, nil
	}

	// Check if manifests are already downloaded and stored in a configmap
	labelSelector := metav1.LabelSelector{
		MatchLabels: p.prepareConfigMapLabels(),
	}

	exists, err := p.checkConfigMapExists(ctx, labelSelector)
	if err != nil {
		return reconcile.Result{}, wrapPhaseError(err, "failed to check that config map with manifests exists", operatorv1.PreflightCheckCondition)
	}

	if exists {
		log.V(5).Info("Config map with downloaded manifests already exists, skip downloading provider manifests")

		return reconcile.Result{}, nil
	}

	repo, err := repositoryFactory(p.providerConfig, p.configClient.Variables())
	if err != nil {
		err = fmt.Errorf("failed to create repo from provider url for provider %q: %w", p.provider.GetName(), err)

		return reconcile.Result{}, wrapPhaseError(err, operatorv1.ComponentsFetchErrorReason, operatorv1.PreflightCheckCondition)
	}

	// Fetch the provider metadata and components yaml files from the provided repository GitHub/GitLab.
	metadataFile, err := repo.GetFile(p.options.Version, metadataFile)
	if err != nil {
		err = fmt.Errorf("failed to read %q from the repository for provider %q: %w", metadataFile, p.provider.GetName(), err)

		return reconcile.Result{}, wrapPhaseError(err, operatorv1.ComponentsFetchErrorReason, operatorv1.PreflightCheckCondition)
	}

	componentsFile, err := repo.GetFile(p.options.Version, repo.ComponentsPath())
	if err != nil {
		err = fmt.Errorf("failed to read %q from the repository for provider %q: %w", componentsFile, p.provider.GetName(), err)

		return reconcile.Result{}, wrapPhaseError(err, operatorv1.ComponentsFetchErrorReason, operatorv1.PreflightCheckCondition)
	}

	if err := p.createManifestsConfigMap(ctx, string(metadataFile), string(componentsFile)); err != nil {
		err = fmt.Errorf("failed to create config map for provider %q: %w", p.provider.GetName(), err)

		return reconcile.Result{}, wrapPhaseError(err, operatorv1.ComponentsFetchErrorReason, operatorv1.PreflightCheckCondition)
	}

	return reconcile.Result{}, nil
}

// checkConfigMapExists checks if a config map exists in Kubernetes with the given LabelSelector.
func (p *phaseReconciler) checkConfigMapExists(ctx context.Context, labelSelector metav1.LabelSelector) (bool, error) {
	labelSet := labels.Set(labelSelector.MatchLabels)
	listOpts := []client.ListOption{
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labelSet)},
	}

	var configMapList corev1.ConfigMapList

	if err := p.ctrlClient.List(ctx, &configMapList, listOpts...); err != nil {
		return false, fmt.Errorf("failed to list ConfigMaps: %w", err)
	}

	if len(configMapList.Items) > 1 {
		return false, fmt.Errorf("more than one config maps were found for given selector: %v", labelSelector.String())
	}

	return len(configMapList.Items) == 1, nil
}

// prepareConfigMapLabels returns labels that identify a config map with downloaded manifests.
func (p *phaseReconciler) prepareConfigMapLabels() map[string]string {
	return map[string]string{
		configMapVersionLabel: p.provider.GetSpec().Version,
		configMapTypeLabel:    p.provider.GetType(),
		configMapNameLabel:    p.provider.GetName(),
		operatorManagedLabel:  "true",
	}
}

// createManifestsConfigMap creates a config map with downloaded manifests.
func (p *phaseReconciler) createManifestsConfigMap(ctx context.Context, metadata, components string) error {
	configMapName := fmt.Sprintf("%s-%s-%s", p.provider.GetType(), p.provider.GetName(), p.provider.GetSpec().Version)

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: p.provider.GetNamespace(),
			Labels:    p.prepareConfigMapLabels(),
		},
		Data: map[string]string{
			metadataConfigMapKey:   metadata,
			componentsConfigMapKey: components,
		},
	}

	gvk := p.provider.GetObjectKind().GroupVersionKind()

	configMap.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
			Name:       p.provider.GetName(),
			UID:        p.provider.GetUID(),
		},
	})

	return p.ctrlClient.Create(ctx, configMap)
}
