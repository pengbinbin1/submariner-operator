/*
SPDX-License-Identifier: Apache-2.0

Copyright Contributors to the Submariner project.

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

package broker

import (
	"context"
	goerrors "errors"
	"time"

	"github.com/pkg/errors"
	"github.com/submariner-io/submariner-operator/internal/component"
	"github.com/submariner-io/submariner-operator/internal/constants"
	"github.com/submariner-io/submariner-operator/internal/rbac"
	"github.com/submariner-io/submariner-operator/pkg/gateway"
	"github.com/submariner-io/submariner-operator/pkg/lighthouse"
	"github.com/submariner-io/submariner-operator/pkg/utils"
	crdutils "github.com/submariner-io/submariner-operator/pkg/utils/crds"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

func Ensure(crdUpdater crdutils.CRDUpdater, kubeClient kubernetes.Interface, componentArr []string, crds bool, namespace string) error {
	if crds {
		for i := range componentArr {
			switch componentArr[i] {
			case component.Connectivity:
				err := gateway.Ensure(crdUpdater)
				if err != nil {
					return errors.Wrap(err, "error setting up the connectivity requirements")
				}
			case component.ServiceDiscovery:
				_, err := lighthouse.Ensure(crdUpdater, lighthouse.BrokerCluster)
				if err != nil {
					return errors.Wrap(err, "error setting up the service discovery requirements")
				}
			case component.Globalnet:
				// Globalnet needs the Lighthouse CRDs too
				_, err := lighthouse.Ensure(crdUpdater, lighthouse.BrokerCluster)
				if err != nil {
					return errors.Wrap(err, "error setting up the globalnet requirements")
				}
			}
		}
	}

	// Create the namespace
	_, err := CreateNewBrokerNamespace(kubeClient, namespace)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "error creating the broker namespace")
	}

	// Create administrator SA, Role, and bind them
	if err := createBrokerAdministratorRoleAndSA(kubeClient, namespace); err != nil {
		return err
	}

	// Create cluster Role, and a default account for backwards compatibility, also bind it
	if err := createBrokerClusterRoleAndDefaultSA(kubeClient, namespace); err != nil {
		return err
	}

	_, err = WaitForClientToken(kubeClient, constants.SubmarinerBrokerAdminSA, namespace)

	return err
}

func createBrokerClusterRoleAndDefaultSA(kubeClient kubernetes.Interface, namespace string) error {
	// Create the a default SA for cluster access (backwards compatibility with documentation)
	_, err := CreateNewBrokerSA(kubeClient, submarinerBrokerClusterDefaultSA, namespace)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "error creating the default broker service account")
	}

	// Create the broker cluster role, which will also be used by any new enrolled cluster
	_, err = CreateOrUpdateClusterBrokerRole(kubeClient, namespace)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "error creating broker role")
	}

	// Create the role binding
	_, err = CreateNewBrokerRoleBinding(kubeClient, submarinerBrokerClusterDefaultSA, submarinerBrokerClusterRole, namespace)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "error creating the broker rolebinding")
	}

	return nil
}

// CreateSAForCluster creates a new SA, and binds it to the submariner cluster role.
func CreateSAForCluster(kubeClient kubernetes.Interface, clusterID, namespace string) (*v1.Secret, error) {
	saName := ClusterSAName(clusterID)

	_, err := CreateNewBrokerSA(kubeClient, saName, namespace)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, errors.Wrap(err, "error creating cluster sa")
	}

	_, err = CreateNewBrokerRoleBinding(kubeClient, saName, submarinerBrokerClusterRole, namespace)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, errors.Wrap(err, "error binding sa to cluster role")
	}

	clientToken, err := WaitForClientToken(kubeClient, saName, namespace)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, errors.Wrap(err, "error getting cluster sa token")
	}

	return clientToken, nil
}

func createBrokerAdministratorRoleAndSA(kubeClient kubernetes.Interface, namespace string) error {
	// Create the SA we need for the managing the broker (from subctl, etc..).
	_, err := CreateNewBrokerSA(kubeClient, constants.SubmarinerBrokerAdminSA, namespace)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "error creating the broker admin service account")
	}

	// Create the broker admin role
	_, err = CreateOrUpdateBrokerAdminRole(kubeClient, namespace)
	if err != nil {
		return errors.Wrap(err, "error creating subctl role")
	}

	// Create the role binding
	_, err = CreateNewBrokerRoleBinding(kubeClient, constants.SubmarinerBrokerAdminSA, submarinerBrokerAdminRole, namespace)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "error creating the broker rolebinding")
	}

	return nil
}

func WaitForClientToken(kubeClient kubernetes.Interface, submarinerBrokerSA, namespace string) (*v1.Secret, error) {
	// wait for the client token to be ready, while implementing
	// exponential backoff pattern, it will wait a total of:
	// sum(n=0..9, 1.2^n * 5) seconds, = 130 seconds
	backoff := wait.Backoff{
		Steps:    10,
		Duration: 5 * time.Second,
		Factor:   1.2,
		Jitter:   1,
	}

	var secret *v1.Secret
	var lastErr error

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		secret, lastErr = rbac.GetClientTokenSecret(kubeClient, namespace, submarinerBrokerSA)
		if lastErr != nil {
			return false, nil // nolint:nilerr // Intentional - the error is propagated via the outer-scoped var 'lastErr'
		}

		return true, nil
	})

	if goerrors.Is(err, wait.ErrWaitTimeout) {
		return nil, lastErr // nolint:wrapcheck // No need to wrap here
	}

	return secret, err // nolint:wrapcheck // No need to wrap here
}

// nolint:wrapcheck // No need to wrap here
func CreateNewBrokerNamespace(kubeClient kubernetes.Interface, namespace string) (brokernamespace *v1.Namespace, err error) {
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}

	return kubeClient.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
}

// nolint:wrapcheck // No need to wrap here
func CreateOrUpdateClusterBrokerRole(kubeClient kubernetes.Interface, namespace string) (created bool, err error) {
	return utils.CreateOrUpdateRole(context.TODO(), kubeClient, namespace, NewBrokerClusterRole())
}

// nolint:wrapcheck // No need to wrap here
func CreateOrUpdateBrokerAdminRole(clientset kubernetes.Interface, namespace string) (created bool, err error) {
	return utils.CreateOrUpdateRole(context.TODO(), clientset, namespace, NewBrokerAdminRole())
}

// nolint:wrapcheck // No need to wrap here
func CreateNewBrokerRoleBinding(kubeClient kubernetes.Interface, serviceAccount, role, namespace string) (
	brokerRoleBinding *rbacv1.RoleBinding, err error) {
	return kubeClient.RbacV1().RoleBindings(namespace).Create(
		context.TODO(), NewBrokerRoleBinding(serviceAccount, role, namespace), metav1.CreateOptions{})
}

// nolint:wrapcheck // No need to wrap here
func CreateNewBrokerSA(kubeClient kubernetes.Interface, submarinerBrokerSA, namespace string) (brokerSA *v1.ServiceAccount, err error) {
	return kubeClient.CoreV1().ServiceAccounts(namespace).Create(
		context.TODO(), NewBrokerSA(submarinerBrokerSA), metav1.CreateOptions{})
}
