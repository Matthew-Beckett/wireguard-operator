/*
Copyright 2021.

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
	"context"
	"fmt"

	ipnetgen "github.com/korylprince/ipnetgen"
	wgtypes "golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	vpnv1alpha1 "github.com/jodevsa/wireguard-operator/api/v1alpha1"
)

// WireguardPeerReconciler reconciles a WireguardPeer object
type WireguardPeerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *WireguardPeerReconciler) secretForPeer(m *vpnv1alpha1.WireguardPeer, privateKey string, publicKey string) *corev1.Secret {
	ls := labelsForWireguard(m.Name)
	dep := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name + "-peer",
			Namespace: m.Namespace,
			Labels:    ls,
		},
		Data: map[string][]byte{"privateKey": []byte(privateKey), "publicKey": []byte(publicKey)},
	}
	// Set Nodered instance as the owner and controller
	ctrl.SetControllerReference(m, dep, r.Scheme)

	return dep

}

//+kubebuilder:rbac:groups=vpn.example.com,resources=wireguardpeers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vpn.example.com,resources=wireguardpeers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vpn.example.com,resources=wireguardpeers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the WireguardPeer object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile

func getAvaialbleIp(cidr string, usedIps []string) (string, error) {
	gen, err := ipnetgen.New(cidr)
	if err != nil {
		return "", err
	}
	for ip := gen.Next(); ip != nil; ip = gen.Next() {
		used := false
		for _, usedIp := range usedIps {
			if ip.String() == usedIp {
				used = true
				break
			}
		}
		if !used {
			return ip.String(), nil
		}
	}

	return "", fmt.Errorf("No available ip found in %s", cidr)
}

func (r *WireguardPeerReconciler) getWireguardPeers(ctx context.Context, req ctrl.Request) (*vpnv1alpha1.WireguardPeerList, error) {
	peers := &vpnv1alpha1.WireguardPeerList{}
	if err := r.List(ctx, peers, client.InNamespace(req.Namespace)); err != nil {
		return nil, err
	}

	relatedPeers := &vpnv1alpha1.WireguardPeerList{}

	for _, peer := range peers.Items {
		relatedPeers.Items = append(relatedPeers.Items, peer)
	}

	return relatedPeers, nil
}

func (r *WireguardPeerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	peer := &vpnv1alpha1.WireguardPeer{}
	err := r.Get(ctx, req.NamespacedName, peer)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("wireguard peer resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get Nodered")
		return ctrl.Result{}, err
	}

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		log.Error(err, "Failed to generate private key")
		return ctrl.Result{}, err
	}

	newPeer := peer.DeepCopy()

	if peer.Spec.Address == "" {
		peers, err := r.getWireguardPeers(ctx, req)

		if err != nil {
			log.Error(err, "Failed to fetch list of peers")
			return ctrl.Result{}, err
		}

		usedIps := []string{"10.8.0.0", "10.8.0.1"}
		for _, p := range peers.Items {
			if p.Spec.WireguardRef != newPeer.Spec.WireguardRef {
				continue
			}
			usedIps = append(usedIps, p.Spec.Address)

		}

		ip, err := getAvaialbleIp("10.8.0.0/24", usedIps)

		if err != nil {
			log.Error(err, "Failed to generate next ip")
			return ctrl.Result{}, err
		}

		newPeer.Spec.Address = ip

		err = r.Update(ctx, newPeer)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if peer.Spec.PublicKey == "" {
		privateKey := key.String()
		publicKey := key.PublicKey().String()

		secret := r.secretForPeer(peer, privateKey, publicKey)

		log.Info("Creating a new secret", "secret.Namespace", secret.Namespace, "secret.Name", secret.Name)
		err = r.Create(ctx, secret)
		if err != nil {
			log.Error(err, "Failed to create new secret", "secret.Namespace", secret.Namespace, "secret.Name", secret.Name)
			return ctrl.Result{}, err
		}

		newPeer.Spec.PublicKey = publicKey
		newPeer.Spec.PrivateKey = vpnv1alpha1.PrivateKey{
			SecretKeyRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: peer.Name + "-peer"}, Key: "privateKey"}}
		err = r.Update(ctx, newPeer)

		if err != nil {
			log.Error(err, "Failed to create new peer", "secret.Namespace", secret.Namespace, "secret.Name", secret.Name)
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil

	}

	wireguard := &vpnv1alpha1.Wireguard{}
	err = r.Get(ctx, types.NamespacedName{Name: newPeer.Spec.WireguardRef, Namespace: newPeer.Namespace}, wireguard)

	if err != nil {
		log.Error(err, "Failed to get wireguard")
		return ctrl.Result{}, err

	}

	if wireguard.Status.Hostname == "" {
		log.Info("Waiting for wireguard to be ready")
		return ctrl.Result{Requeue: true}, nil
	}

	wireguardSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: newPeer.Spec.WireguardRef, Namespace: newPeer.Namespace}, wireguardSecret)

	if len(newPeer.OwnerReferences) == 0 {
		log.Info("Waiting for owner reference to be set " + wireguard.Name + " " + newPeer.Name)
		ctrl.SetControllerReference(wireguard, newPeer, r.Scheme)

		if err != nil {
			log.Error(err, "Failed to update peer with controller reference")
			return ctrl.Result{}, err
		}

		r.Update(ctx, newPeer)

		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WireguardPeerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vpnv1alpha1.WireguardPeer{}).
		Complete(r)
}
