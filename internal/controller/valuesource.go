/*
Copyright 2026.

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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "idp.y.digital/dex-idp-operator/api/v1alpha1"
)

// resolveValueSource resolves a ValueSource to its inline value or a Secret key in namespace.
func resolveValueSource(
	ctx context.Context,
	c client.Client,
	namespace string,
	source *idpv1alpha1.ValueSource,
) ([]byte, error) {
	if source == nil {
		return nil, fmt.Errorf("value source is required")
	}
	if source.Value != nil {
		return []byte(*source.Value), nil
	}
	if source.ValueFrom == nil {
		return nil, fmt.Errorf("value or valueFrom is required")
	}

	ref := source.ValueFrom.SecretKeyRef
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	if err := c.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) && ref.Optional != nil && *ref.Optional {
			return nil, nil
		}
		return nil, err
	}

	value, ok := secret.Data[ref.Key]
	if !ok {
		if ref.Optional != nil && *ref.Optional {
			return nil, nil
		}
		return nil, fmt.Errorf("secret %q does not contain key %q", ref.Name, ref.Key)
	}

	return value, nil
}

// valueSourceReferencesSecret reports whether source resolves from the named Secret.
func valueSourceReferencesSecret(source *idpv1alpha1.ValueSource, secretName string) bool {
	return source != nil &&
		source.ValueFrom != nil &&
		source.ValueFrom.SecretKeyRef.Name == secretName
}
