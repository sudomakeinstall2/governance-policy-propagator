// Copyright Contributors to the Open Cluster Management project

package policystatus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	appsv1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/apps/placementrule/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	policiesv1 "open-cluster-management.io/governance-policy-propagator/api/v1"
	policiesv1beta1 "open-cluster-management.io/governance-policy-propagator/api/v1beta1"
	"open-cluster-management.io/governance-policy-propagator/controllers/common"
	"open-cluster-management.io/governance-policy-propagator/controllers/propagator"
)

const ControllerName string = "root-policy-status"

var log = ctrl.Log.WithName(ControllerName)

//+kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policies,verbs=get;list;watch
//+kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policies/status,verbs=get;update;patch

// SetupWithManager sets up the controller with the Manager.
func (r *RootPolicyStatusReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrentReconciles uint) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: int(maxConcurrentReconciles)}).
		Named(ControllerName).
		For(
			&policiesv1.Policy{},
			builder.WithPredicates(common.NeverEnqueue),
		).
		Watches(
			&policiesv1.PlacementBinding{},
			handler.EnqueueRequestsFromMapFunc(mapBindingToPolicies(mgr.GetClient())),
		).
		Watches(
			&appsv1.PlacementRule{},
			handler.EnqueueRequestsFromMapFunc(mapRuleToPolicies(mgr.GetClient())),
		).
		Watches(
			&clusterv1beta1.PlacementDecision{},
			handler.EnqueueRequestsFromMapFunc(mapDecisionToPolicies(mgr.GetClient())),
		).
		// This is a workaround - the controller-runtime requires a "For", but does not allow it to
		// modify the eventhandler. Currently we need to enqueue requests for Policies in a very
		// particular way, so we will define that in a separate "Watches"
		Watches(
			&policiesv1.Policy{},
			handler.EnqueueRequestsFromMapFunc(common.MapToRootPolicy(mgr.GetClient())),
			builder.WithPredicates(policyStatusPredicate()),
		).
		Complete(r)
}

// blank assignment to verify that RootPolicyStatusReconciler implements reconcile.Reconciler
var _ reconcile.Reconciler = &RootPolicyStatusReconciler{}

// RootPolicyStatusReconciler handles replicated policy status updates and updates the root policy status.
type RootPolicyStatusReconciler struct {
	client.Client
	// Use a shared lock with the main policy controller to avoid conflicting updates.
	RootPolicyLocks *sync.Map
	Scheme          *runtime.Scheme
}

// Reconcile will update the root policy status based on the current state whenever a root or replicated policy status
// is updated. The reconcile request is always on the root policy. This approach is taken rather than just handling a
// single replicated policy status per reconcile to be able to "batch" status update requests when there are bursts of
// replicated policy status updates. This lowers resource utilization on the controller and the Kubernetes API server.
func (r *RootPolicyStatusReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	log.V(1).Info("Reconciling the root policy status")

	log.V(3).Info("Acquiring the lock for the root policy")

	lock, _ := r.RootPolicyLocks.LoadOrStore(request.NamespacedName, &sync.Mutex{})

	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	rootPolicy := &policiesv1.Policy{}

	err := r.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: request.Name}, rootPolicy)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			log.V(2).Info("The root policy has been deleted. Doing nothing.")

			return reconcile.Result{}, nil
		}

		log.Error(err, "Failed to get the root policy")

		return reconcile.Result{}, err
	}

	log.Info("Updating the root policy status")

	err = r.rootStatusUpdate(rootPolicy) //nolint:contextcheck
	if err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *RootPolicyStatusReconciler) rootStatusUpdate(rootPolicy *policiesv1.Policy) error {
	placements, decisions, err := r.getDecisions(rootPolicy)
	if err != nil {
		log.Info("Failed to get any placement decisions. Giving up on the request.")

		return errors.New("could not get the placement decisions")
	}

	cpcs, cpcsErr := r.calculatePerClusterStatus(rootPolicy, decisions)
	if cpcsErr != nil {
		// If there is a new replicated policy, then its lookup is expected to fail - it hasn't been created yet.
		log.Error(cpcsErr, "Failed to get at least one replicated policy, but that may be expected. Ignoring.")
	}

	err = r.Get(context.TODO(),
		types.NamespacedName{
			Namespace: rootPolicy.Namespace,
			Name:      rootPolicy.Name,
		}, rootPolicy)
	if err != nil {
		log.Error(err, "Failed to refresh the cached policy. Will use existing policy.")
	}

	// make a copy of the original status
	originalCPCS := make([]*policiesv1.CompliancePerClusterStatus, len(rootPolicy.Status.Status))
	copy(originalCPCS, rootPolicy.Status.Status)

	rootPolicy.Status.Status = cpcs
	rootPolicy.Status.ComplianceState = propagator.CalculateRootCompliance(cpcs)
	rootPolicy.Status.Placement = placements

	err = r.Status().Update(context.TODO(), rootPolicy)
	if err != nil {
		return err
	}

	return nil
}

// getPolicyPlacementDecisions retrieves the placement decisions for a input PlacementBinding when
// the policy is bound within it. It can return an error if the PlacementBinding is invalid, or if
// a required lookup fails.
func (r *RootPolicyStatusReconciler) getPolicyPlacementDecisions(
	instance *policiesv1.Policy, pb *policiesv1.PlacementBinding,
) (decisions []appsv1.PlacementDecision, placements []*policiesv1.Placement, err error) {
	if !common.HasValidPlacementRef(pb) {
		return nil, nil, fmt.Errorf("placement binding %s/%s reference is not valid", pb.Name, pb.Namespace)
	}

	policySubjectFound := false
	policySetSubjects := make(map[string]struct{}) // a set, to prevent duplicates

	for _, subject := range pb.Subjects {
		if subject.APIGroup != policiesv1.SchemeGroupVersion.Group {
			continue
		}

		switch subject.Kind {
		case policiesv1.Kind:
			if !policySubjectFound && subject.Name == instance.GetName() {
				policySubjectFound = true

				placements = append(placements, &policiesv1.Placement{
					PlacementBinding: pb.GetName(),
				})
			}
		case policiesv1.PolicySetKind:
			if _, exists := policySetSubjects[subject.Name]; !exists {
				policySetSubjects[subject.Name] = struct{}{}

				if r.isPolicyInPolicySet(instance.GetName(), subject.Name, pb.GetNamespace()) {
					placements = append(placements, &policiesv1.Placement{
						PlacementBinding: pb.GetName(),
						PolicySet:        subject.Name,
					})
				}
			}
		}
	}

	if len(placements) == 0 {
		// None of the subjects in the PlacementBinding were relevant to this Policy.
		return nil, nil, nil
	}

	// If the placementRef exists, then it needs to be added to the placement item
	refNN := types.NamespacedName{
		Namespace: pb.GetNamespace(),
		Name:      pb.PlacementRef.Name,
	}

	switch pb.PlacementRef.Kind {
	case "PlacementRule":
		plr := &appsv1.PlacementRule{}
		if err := r.Get(context.TODO(), refNN, plr); err != nil && !k8serrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to check for PlacementRule '%v': %w", pb.PlacementRef.Name, err)
		}

		for i := range placements {
			placements[i].PlacementRule = plr.Name // will be empty if the PlacementRule was not found
		}
	case "Placement":
		pl := &clusterv1beta1.Placement{}
		if err := r.Get(context.TODO(), refNN, pl); err != nil && !k8serrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to check for Placement '%v': %w", pb.PlacementRef.Name, err)
		}

		for i := range placements {
			placements[i].Placement = pl.Name // will be empty if the Placement was not found
		}
	}

	// If there are no placements, then the PlacementBinding is not for this Policy.
	if len(placements) == 0 {
		return nil, nil, nil
	}

	// If the policy is disabled, don't return any decisions, so that the policy isn't put on any clusters
	if instance.Spec.Disabled {
		return nil, placements, nil
	}

	decisions, err = common.GetDecisions(r.Client, pb)

	return decisions, placements, err
}

// getAllClusterDecisions calculates which managed clusters should have a replicated policy, and
// whether there are any BindingOverrides for that cluster. The placements array it returns is
// sorted by PlacementBinding name. It can return an error if the PlacementBinding is invalid, or if
// a required lookup fails.
func (r *RootPolicyStatusReconciler) getAllClusterDecisions(
	instance *policiesv1.Policy, pbList *policiesv1.PlacementBindingList,
) (
	decisions map[appsv1.PlacementDecision]policiesv1.BindingOverrides, placements []*policiesv1.Placement, err error,
) {
	decisions = make(map[appsv1.PlacementDecision]policiesv1.BindingOverrides)

	// Process all placement bindings without subFilter
	for i, pb := range pbList.Items {
		if pb.SubFilter == policiesv1.Restricted {
			continue
		}

		plcDecisions, plcPlacements, err := r.getPolicyPlacementDecisions(instance, &pbList.Items[i])
		if err != nil {
			return nil, nil, err
		}

		if len(plcDecisions) == 0 {
			log.Info("No placement decisions to process for this policy from this binding",
				"policyName", instance.GetName(), "bindingName", pb.GetName())
		}

		for _, decision := range plcDecisions {
			if overrides, ok := decisions[decision]; ok {
				// Found cluster in the decision map
				if strings.EqualFold(pb.BindingOverrides.RemediationAction, string(policiesv1.Enforce)) {
					overrides.RemediationAction = strings.ToLower(string(policiesv1.Enforce))
					decisions[decision] = overrides
				}
			} else {
				// No found cluster in the decision map, add it to the map
				decisions[decision] = policiesv1.BindingOverrides{
					// empty string if pb.BindingOverrides.RemediationAction is not defined
					RemediationAction: strings.ToLower(pb.BindingOverrides.RemediationAction),
				}
			}
		}

		placements = append(placements, plcPlacements...)
	}

	if len(decisions) == 0 {
		sort.Slice(placements, func(i, j int) bool {
			return placements[i].PlacementBinding < placements[j].PlacementBinding
		})

		// No decisions, and subfilters can't add decisions, so we can stop early.
		return nil, placements, nil
	}

	// Process all placement bindings with subFilter:restricted
	for i, pb := range pbList.Items {
		if pb.SubFilter != policiesv1.Restricted {
			continue
		}

		foundInDecisions := false

		plcDecisions, plcPlacements, err := r.getPolicyPlacementDecisions(instance, &pbList.Items[i])
		if err != nil {
			return nil, nil, err
		}

		if len(plcDecisions) == 0 {
			log.Info("No placement decisions to process for this policy from this binding",
				"policyName", instance.GetName(), "bindingName", pb.GetName())
		}

		for _, decision := range plcDecisions {
			if overrides, ok := decisions[decision]; ok {
				// Found cluster in the decision map
				foundInDecisions = true

				if strings.EqualFold(pb.BindingOverrides.RemediationAction, string(policiesv1.Enforce)) {
					overrides.RemediationAction = strings.ToLower(string(policiesv1.Enforce))
					decisions[decision] = overrides
				}
			}
		}

		if foundInDecisions {
			placements = append(placements, plcPlacements...)
		}
	}

	sort.Slice(placements, func(i, j int) bool {
		return placements[i].PlacementBinding < placements[j].PlacementBinding
	})

	return decisions, placements, nil
}

type decisionSet map[appsv1.PlacementDecision]bool

// getDecisions identifies all managed clusters which should have a replicated policy
func (r *RootPolicyStatusReconciler) getDecisions(
	instance *policiesv1.Policy,
) (
	[]*policiesv1.Placement, decisionSet, error,
) {
	log := log.WithValues("policyName", instance.GetName(), "policyNamespace", instance.GetNamespace())
	decisions := make(map[appsv1.PlacementDecision]bool)

	pbList := &policiesv1.PlacementBindingList{}

	err := r.List(context.TODO(), pbList, &client.ListOptions{Namespace: instance.GetNamespace()})
	if err != nil {
		log.Error(err, "Could not list the placement bindings")

		return nil, decisions, err
	}

	allClusterDecisions, placements, err := r.getAllClusterDecisions(instance, pbList)
	if err != nil {
		return placements, decisions, err
	}

	if allClusterDecisions == nil {
		allClusterDecisions = make(map[appsv1.PlacementDecision]policiesv1.BindingOverrides)
	}

	for dec := range allClusterDecisions {
		decisions[dec] = true
	}

	return placements, decisions, nil
}

func (r *RootPolicyStatusReconciler) calculatePerClusterStatus(
	instance *policiesv1.Policy, decisions decisionSet,
) ([]*policiesv1.CompliancePerClusterStatus, error) {
	if instance.Spec.Disabled {
		return nil, nil
	}

	status := make([]*policiesv1.CompliancePerClusterStatus, 0, len(decisions))
	var lookupErr error // save until end, to attempt all lookups

	// Update the status based on the processed decisions
	for dec := range decisions {
		replicatedPolicy := &policiesv1.Policy{}
		key := types.NamespacedName{
			Namespace: dec.ClusterNamespace, Name: instance.Namespace + "." + instance.Name,
		}

		err := r.Get(context.TODO(), key, replicatedPolicy)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				status = append(status, &policiesv1.CompliancePerClusterStatus{
					ClusterName:      dec.ClusterName,
					ClusterNamespace: dec.ClusterNamespace,
				})

				continue
			}

			lookupErr = err
		}

		status = append(status, &policiesv1.CompliancePerClusterStatus{
			ComplianceState:  replicatedPolicy.Status.ComplianceState,
			ClusterName:      dec.ClusterName,
			ClusterNamespace: dec.ClusterNamespace,
		})
	}

	sort.Slice(status, func(i, j int) bool {
		return status[i].ClusterName < status[j].ClusterName
	})

	return status, lookupErr
}

func (r *RootPolicyStatusReconciler) isPolicyInPolicySet(policyName, policySetName, namespace string) bool {
	log := log.WithValues("policyName", policyName, "policySetName", policySetName, "policyNamespace", namespace)

	policySet := policiesv1beta1.PolicySet{}
	setNN := types.NamespacedName{
		Name:      policySetName,
		Namespace: namespace,
	}

	if err := r.Get(context.TODO(), setNN, &policySet); err != nil {
		log.Error(err, "Failed to get the policyset")

		return false
	}

	for _, plc := range policySet.Spec.Policies {
		if string(plc) == policyName {
			return true
		}
	}

	return false
}
