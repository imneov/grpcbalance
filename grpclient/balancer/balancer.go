package balancer

import (
	"errors"
	"fmt"
	"grpcbalance/grpclient/picker"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/resolver"
)

type baseBalancer struct {
	cc            balancer.ClientConn
	pickerBuilder picker.PickerBuilder

	csEvltr *balancer.ConnectivityStateEvaluator
	state   connectivity.State

	subConns map[resolver.Address]balancer.SubConn
	scStates map[balancer.SubConn]connectivity.State
	picker   balancer.Picker
	config   Config

	resolverErr error // the last error reported by the resolver; cleared on successful resolution
	connErr     error // the last connection error; cleared upon leaving TransientFailure
}

// Config contains the config info about the base balancer builder.
type Config struct {
	// HealthCheck indicates whether health checking should be enabled for this specific balancer.
	HealthCheck bool

	// `attributes` is stripped from the keys of this map (the addresses)
	StripAttributes bool
}

func (b *baseBalancer) ResolverError(err error) {
	b.resolverErr = err
	if len(b.subConns) == 0 {
		b.state = connectivity.TransientFailure
	}

	if b.state != connectivity.TransientFailure {
		// The picker will not change since the balancer does not currently
		// report an error.
		return
	}
	b.regeneratePicker()
	b.cc.UpdateState(balancer.State{
		ConnectivityState: b.state,
		Picker:            b.picker,
	})
}

func (b *baseBalancer) UpdateClientConnState(s balancer.ClientConnState) error {
	// Successful resolution; clear resolver error and ensure we return nil.
	b.resolverErr = nil
	// addrsSet is the set converted from addrs, it's used for quick lookup of an address.
	addrsSet := make(map[resolver.Address]struct{})
	for _, a := range s.ResolverState.Addresses {
		// Strip attributes from addresses before using them as map keys. So
		// that when two addresses only differ in attributes pointers (but with
		// the same attribute content), they are considered the same address.
		//
		// Note that this doesn't handle the case where the attribute content is
		// different. So if users want to set different attributes to create
		// duplicate connections to the same backend, it doesn't work. This is
		// fine for now, because duplicate is done by setting Metadata today.

		aNoAttrs := a

		// read attributes to handle duplicate connections.
		if b.config.StripAttributes {
			aNoAttrs.Attributes = nil
		}

		addrsSet[aNoAttrs] = struct{}{}
		if sc, ok := b.subConns[aNoAttrs]; !ok {
			// a is a new address (not existing in b.subConns).
			//
			// When creating SubConn, the original address with attributes is
			// passed through. So that connection configurations in attributes
			// (like creds) will be used.
			sc, err := b.cc.NewSubConn([]resolver.Address{a}, balancer.NewSubConnOptions{HealthCheckEnabled: b.config.HealthCheck})
			if err != nil {
				continue
			}
			b.subConns[aNoAttrs] = sc
			b.scStates[sc] = connectivity.Idle
			sc.Connect()
		} else {
			// Always update the subconn's address in case the attributes
			// changed.
			//
			// The SubConn does a reflect.DeepEqual of the new and old
			// addresses. So this is a noop if the current address is the same
			// as the old one (including attributes).
			sc.UpdateAddresses([]resolver.Address{a})
		}
	}
	for a, sc := range b.subConns {
		// a was removed by resolver.
		if _, ok := addrsSet[a]; !ok {
			b.cc.RemoveSubConn(sc)
			delete(b.subConns, a)
			// Keep the state of this sc in b.scStates until sc's state becomes Shutdown.
			// The entry will be deleted in UpdateSubConnState.
		}
	}
	// If resolver state contains no addresses, return an error so ClientConn
	// will trigger re-resolve. Also records this as an resolver error, so when
	// the overall state turns transient failure, the error message will have
	// the zero address information.
	if len(s.ResolverState.Addresses) == 0 {
		b.ResolverError(errors.New("produced zero addresses"))
		// will call resolver.ResolveNow
		return balancer.ErrBadResolverState
	}
	return nil
}

// mergeErrors builds an error from the last connection error and the last
// resolver error.  Must only be called if b.state is TransientFailure.
func (b *baseBalancer) mergeErrors() error {
	// connErr must always be non-nil unless there are no SubConns, in which
	// case resolverErr must be non-nil.
	if b.connErr == nil {
		return fmt.Errorf("last resolver error: %v", b.resolverErr)
	}
	if b.resolverErr == nil {
		return fmt.Errorf("last connection error: %v", b.connErr)
	}
	return fmt.Errorf("last connection error: %v; last resolver error: %v", b.connErr, b.resolverErr)
}

// regeneratePicker takes a snapshot of the balancer, and generates a picker
// from it. The picker is
//  - errPicker if the balancer is in TransientFailure,
//  - built by the pickerBuilder with all READY SubConns otherwise.
func (b *baseBalancer) regeneratePicker() {
	if b.state == connectivity.TransientFailure {
		b.picker = picker.NewErrPicker(b.mergeErrors())
		return
	}
	readySCs := make(map[balancer.SubConn]picker.SubConnInfo)

	// Filter out all ready SCs from full subConn map.
	for addr, sc := range b.subConns {
		if st, ok := b.scStates[sc]; ok && st == connectivity.Ready {
			readySCs[sc] = picker.SubConnInfo{Address: addr}
		}
	}
	b.picker = b.pickerBuilder.Build(picker.PickerBuildInfo{ReadySCs: readySCs})
}

func (b *baseBalancer) UpdateSubConnState(sc balancer.SubConn, state balancer.SubConnState) {
	s := state.ConnectivityState
	oldS, ok := b.scStates[sc]
	if !ok {
		return
	}
	if oldS == connectivity.TransientFailure && s == connectivity.Connecting {
		// Once a subconn enters TRANSIENT_FAILURE, ignore subsequent
		// CONNECTING transitions to prevent the aggregated state from being
		// always CONNECTING when many backends exist but are all down.
		return
	}
	b.scStates[sc] = s
	switch s {
	case connectivity.Idle:
		sc.Connect()
	case connectivity.Shutdown:
		// When an address was removed by resolver, b called RemoveSubConn but
		// kept the sc's state in scStates. Remove state for this sc here.
		delete(b.scStates, sc)
	case connectivity.TransientFailure:
		// Save error to be reported via picker.
		b.connErr = state.ConnectionError
	}

	b.state = b.csEvltr.RecordTransition(oldS, s)

	// Regenerate picker when one of the following happens:
	//  - this sc entered or left ready
	//  - the aggregated state of balancer is TransientFailure
	//    (may need to update error message)
	if (s == connectivity.Ready) != (oldS == connectivity.Ready) ||
		b.state == connectivity.TransientFailure {
		b.regeneratePicker()
	}

	b.cc.UpdateState(balancer.State{ConnectivityState: b.state, Picker: b.picker})
}

// Close is a nop because base balancer doesn't have internal state to clean up,
// and it doesn't need to call RemoveSubConn for the SubConns.
func (b *baseBalancer) Close() {
}
