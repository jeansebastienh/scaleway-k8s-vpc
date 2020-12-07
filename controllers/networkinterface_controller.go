/*


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
	"net"
	"time"

	"github.com/go-logr/logr"
	instance "github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	vpcv1alpha1 "github.com/Sh4d1/scaleway-k8s-vpc/api/v1alpha1"
	"github.com/Sh4d1/scaleway-k8s-vpc/pkg/nics"
)

// NetworkInterfaceReconciler reconciles a NetworkInterface object
type NetworkInterfaceReconciler struct {
	client.Client
	Log         logr.Logger
	Scheme      *runtime.Scheme
	MetadataAPI *instance.MetadataAPI
	NodeName    string
	NICs        *nics.NICs
}

// +kubebuilder:rbac:groups=vpc.scaleway.com,resources=networkinterfaces,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=vpc.scaleway.com,resources=networkinterfaces/status,verbs=get;update
// +kubebuilder:rbac:groups=vpc.scaleway.com,resources=privatenetworks,verbs=get;list;watch

func (r *NetworkInterfaceReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("networkinterface", req.NamespacedName)

	nic := &vpcv1alpha1.NetworkInterface{}

	err := r.Client.Get(ctx, req.NamespacedName, nic)
	if err != nil {
		log.Error(err, "could not find object")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if nic.Spec.NodeName != r.NodeName {
		return ctrl.Result{}, nil
	}
	if nic.Status.MacAddress == "" {
		return ctrl.Result{RequeueAfter: time.Second * 1}, nil
	}

	if !nic.ObjectMeta.GetDeletionTimestamp().IsZero() {
		if controllerutil.ContainsFinalizer(nic, finalizerName) {
			err := r.NICs.TearDownLink(nic.Status.MacAddress, nic.Spec.Address)
			if err != nil {
				log.Error(err, "unable to tear down link")
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(nic, finalizerName)
			err = r.Client.Update(ctx, nic)
			if err != nil {
				log.Error(err, fmt.Sprintf("failed to patch networkInterface %s", nic.Name))
				return ctrl.Result{}, err
			}
		}
	}

	md, err := r.MetadataAPI.GetMetadata()
	if err != nil {
		log.Error(err, "unable to get metadata")
		return ctrl.Result{}, err
	}

	found := false
	for _, n := range md.PrivateNICs {
		if n.MacAddress == nic.Status.MacAddress {
			found = true
			break
		}
	}
	if !found {
		err := fmt.Errorf("nic not found on node")
		log.Error(err, "unable to find nic")
		return ctrl.Result{}, err
	}

	linkName, err := r.NICs.GetLinkName(nic.Status.MacAddress)
	if err != nil {
		log.Error(err, "unable to get link")
		return ctrl.Result{}, err
	}

	nic.Status.LinkName = linkName
	err = r.Client.Status().Update(ctx, nic)
	if err != nil {
		log.Error(err, "unable to update status")
		return ctrl.Result{}, err
	}

	err = r.NICs.ConfigureLink(nic.Status.MacAddress, nic.Spec.Address)
	if err != nil {
		log.Error(err, "unable to configure link")
		return ctrl.Result{}, err
	}

	pnet := vpcv1alpha1.PrivateNetwork{}
	err = r.Client.Get(ctx, types.NamespacedName{Name: nic.OwnerReferences[0].Name}, &pnet)
	if err != nil {
		log.Error(err, "unable to get private network")
		return ctrl.Result{}, err
	}

	routes := []nics.Route{}
	for _, route := range pnet.Spec.Routes {
		via := net.ParseIP(route.Via)
		to, err := netlink.ParseIPNet(route.To)
		if err != nil {
			log.Error(err, fmt.Sprintf("unable to parse to route %s", route.To))
			return ctrl.Result{}, err
		}
		routes = append(routes, nics.Route{
			To:  to,
			Via: via,
		})
	}

	err = r.NICs.SyncRoutes(nic.Status.MacAddress, routes)
	if err != nil {
		log.Error(err, "unable to sync routes")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *NetworkInterfaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vpcv1alpha1.NetworkInterface{}).
		Watches(&source.Kind{
			Type: &vpcv1alpha1.PrivateNetwork{},
		}, &handler.Funcs{
			UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
				r.Log.Info("got update PrivateNetwork event")
				nicsList := &vpcv1alpha1.NetworkInterfaceList{}
				err := r.Client.List(context.Background(), nicsList,
					client.MatchingLabels{
						privateNetworkLabel: e.MetaNew.GetName(),
					},
				)
				if err != nil {
					r.Log.Error(err, "unable to sync nics on privateNetwork update")
					return
				}
				for _, nic := range nicsList.Items {
					r.Log.Info(fmt.Sprintf("adding event for nic %s", nic.Name))
					q.Add(reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name: nic.Name,
						},
					})
				}
			},
		}).
		Complete(r)
}
