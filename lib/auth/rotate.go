/*
Copyright 2018 Gravitational, Inc.

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

package auth

import (
	"crypto/x509/pkix"
	"time"

	"github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/tlsca"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/pborman/uuid"
	"github.com/sirupsen/logrus"
)

// RotateRequest is a request to start rotation of the certificate authority
type RotateRequest struct {
	// Type is certificate authority type, if omitted, both will be rotated
	Type services.CertAuthType `json:"type"`
	// GracePeriod is optional grace period, if omitted, default is set,
	// if 0 is supplied, means force rotate all certificate authorities
	// right away.
	GracePeriod *time.Duration `json:"grace_period,omitempty"`
	// TargetPhase sets desired rotation phase to move to, if not set
	// will be set automatically, is a required argument
	// for manual rotation.
	TargetPhase string `json:"target_phase,omitempty"`
	// Mode sets manual mode with manually updated phases,
	// otherwise phases are set automatically
	Mode string `json:"mode"`
	// Schedule is an optional rotation schedule,
	// autogenerated if not set
	Schedule *services.RotationSchedule `json:"schedule"`
}

// Types returns cert authority types requested to rotate
func (r *RotateRequest) Types() []services.CertAuthType {
	switch r.Type {
	case "":
		return []services.CertAuthType{services.HostCA, services.UserCA}
	case services.HostCA:
		return []services.CertAuthType{services.HostCA}
	case services.UserCA:
		return []services.CertAuthType{services.UserCA}
	}
	return nil
}

// CheckAndSetDefaults checks and sets defaults
func (r *RotateRequest) CheckAndSetDefaults(clock clockwork.Clock) error {
	if r.TargetPhase == "" {
		// if phase if not set, imply that the first meaningful phase
		// is set as a target phase
		r.TargetPhase = services.RotationPhaseUpdateClients
	}
	// if mode is not set, default to manual (as it's safer)
	if r.Mode == "" {
		r.Mode = services.RotationModeManual
	}
	switch r.Type {
	case "", services.HostCA, services.UserCA:
	default:
		return trace.BadParameter("unsupported certificate authority type: %q", r.Type)
	}
	if r.GracePeriod == nil {
		period := defaults.RotationGracePeriod
		r.GracePeriod = &period
	}
	if r.Schedule == nil {
		var err error
		r.Schedule, err = services.GenerateSchedule(clock, *r.GracePeriod)
		if err != nil {
			return trace.Wrap(err)
		}
	} else {
		if err := r.Schedule.CheckAndSetDefaults(clock); err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

// rotationReq is an internal rotation requrest
type rotationReq struct {
	// clock is set by the auth server internally
	clock clockwork.Clock
	// ca is a certificate authority to rotate, set by the auth server internally
	ca services.CertAuthority
	// targetPhase is a target rotation phase to set
	targetPhase string
	// mode is a rotation mode
	mode string
	// gracePeriod is a rotation grace period
	gracePeriod time.Duration
	// schedule is a schedule to set
	schedule services.RotationSchedule
}

// RotateCertAuthority starts or restarts certificate rotation process
//
// Rotation procedure description
// ------------------------------
//
// Rotation procedure is based on the state machine approach.
//
// Here are the supported rotation states:
//
// * Standby - the system is in standby mode and ready to take action.
// * In-progress - rotation is in progress.
//
// In-progress state is split into multiple phases and the system
// can traverse between phases using supported transitions.
//
// Here are the supported phases:
//
//  * Standby - no action is taken.
//
//  * Update Clients - new CA is issued, all internal system clients
//  have to reconnect and receive the new credentials, but all servers
//  TLS, SSH and Proxies will still use old clients. Certs from old CA and new CA
//  are trusted within the system. This phase is necessary so old clients
//  can receive new credentials from the auth server. If this phase did not
//  exist, old clients could not trust servers serving new credentials, because
//  old clients did not receive new information yet. It is possible to transition
//  from this phase to phase "Update servers" or "Rollback".
//
//  * Update Servers - all internal system components reload, and use
//  new credentials both in the internal clients and servers, however
// old CA issued credentials are still trusted. This is done to make it possible
// for old components to be visible within the system. It is possible to transition
// from this phase to "Rollback" or "Standby". When transitioning to "Standby"
// phase, the rotation is considered completed, old CA is removed from the system
// and components reload again, but this time they don't trust old CA any more.
//
//
// * Rollback phase is used to revert any changes. When going to rollback phase
// the newly issued CA is trusted, so components can reload and receive "old"
// credentials, but all credentials are issued using "old" certificate authority.
// This phase is useful when administrator makes a mistake, or there are some
// offline components that will loose the connection in case if rotation
// completes. It is only possible to transition from this phase to "Standby".
// When transitioning to "Standby" phase from "Rollback" phase, all components
// reload, but "new" CA is discarded and no longer trusted, so system
// goes back to the original state.
//
//
// Rotation modes
// --------------
//
// There are two rotation modes supported - manual or automatic.
//
// * Manual mode allows administrators to transition between
// phases explicitly setting a phase on every request to this method.
// This gives admins more control over rotation schedule.
//
// * Automatic mode performs automatic transition between phases
// on a given schedule. Schedule is a simple time table
// that specifies exact date when the next phase should take place. If automatic
// transition between any phase fails, it switches back to manual mode and stops
// execution phases on the schedule. If schedule is not specified,
// it will be auto generated based on "grace period" duration parameter,
// time between all phases will be evenly split over the grace period duration.
//
// It is possible to switch from automatic to manual by setting the phase
// to rollback, this action will switch mode to manual.
//
//
func (a *AuthServer) RotateCertAuthority(req RotateRequest) error {
	if err := req.CheckAndSetDefaults(a.clock); err != nil {
		return trace.Wrap(err)
	}
	clusterName := a.clusterName.GetClusterName()

	caTypes := req.Types()
	for _, caType := range caTypes {
		existing, err := a.GetCertAuthority(services.CertAuthID{
			Type:       caType,
			DomainName: clusterName,
		}, true)
		if err != nil {
			return trace.Wrap(err)
		}
		rotated, err := processRotationRequest(rotationReq{
			ca:          existing,
			clock:       a.clock,
			targetPhase: req.TargetPhase,
			schedule:    *req.Schedule,
			gracePeriod: *req.GracePeriod,
			mode:        req.Mode,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		if err := a.CompareAndSwapCertAuthority(rotated, existing); err != nil {
			return trace.Wrap(err)
		}
		rotation := rotated.GetRotation()
		switch rotation.State {
		case services.RotationStateInProgress:
			log.WithFields(logrus.Fields{"type": caType}).Infof("Rotation is in progress, current phase: %q.", rotation.Phase)
		case services.RotationStateStandby:
			log.WithFields(logrus.Fields{"type": caType}).Infof("Rotation has been completed.")
		}
	}
	return nil
}

// RotateExternalCertAuthority rotates external certificate authority,
// this method is called by remote trusted cluster and is used to update
// only public keys and certificates of the certificate authority.
func (a *AuthServer) RotateExternalCertAuthority(ca services.CertAuthority) error {
	if ca == nil {
		return trace.BadParameter("missing certificate authority")
	}
	// this is just an extra precaution against local admins,
	// because this is additionally enforced by RBAC as well
	if ca.GetClusterName() == a.clusterName.GetClusterName() {
		return trace.BadParameter("can not rotate local certificate authority")
	}

	existing, err := a.GetCertAuthority(services.CertAuthID{
		Type:       ca.GetType(),
		DomainName: ca.GetClusterName(),
	}, false)
	if err != nil {
		return trace.Wrap(err)
	}

	updated := existing.Clone()
	updated.SetCheckingKeys(ca.GetCheckingKeys())
	updated.SetTLSKeyPairs(ca.GetTLSKeyPairs())
	updated.SetRotation(ca.GetRotation())

	// use compare and swap to protect from concurrent updates
	// by trusted cluster API
	if err := a.CompareAndSwapCertAuthority(updated, existing); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// autoRotateCertAuthorities automatically rotates cert authorities,
// does nothing if no rotation parameters were set up
// or it is too early to rotate per schedule
func (a *AuthServer) autoRotateCertAuthorities() error {
	clusterName := a.clusterName.GetClusterName()
	for _, caType := range []services.CertAuthType{services.HostCA, services.UserCA} {
		ca, err := a.GetCertAuthority(services.CertAuthID{
			Type:       caType,
			DomainName: clusterName,
		}, true)
		if err != nil {
			return trace.Wrap(err)
		}
		if err := a.autoRotate(ca); err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

func (a *AuthServer) autoRotate(ca services.CertAuthority) error {
	rotation := ca.GetRotation()
	// rotation mode is not manual, nothing to do
	if rotation.Mode != services.RotationModeAuto {
		return nil
	}
	// rotation is not in progress, there is nothing to do
	if rotation.State != services.RotationStateInProgress {
		return nil
	}
	logger := log.WithFields(logrus.Fields{"type": ca.GetType()})
	var req *rotationReq
	switch rotation.Phase {
	case services.RotationPhaseUpdateClients:
		if rotation.Schedule.UpdateServers.After(a.clock.Now()) {
			return nil
		}
		req = &rotationReq{
			clock:       a.clock,
			ca:          ca,
			targetPhase: services.RotationPhaseUpdateServers,
			mode:        services.RotationModeAuto,
			gracePeriod: rotation.GracePeriod.Duration,
			schedule:    rotation.Schedule,
		}
	case services.RotationPhaseUpdateServers:
		if rotation.Schedule.Standby.After(a.clock.Now()) {
			return nil
		}
		req = &rotationReq{
			clock:       a.clock,
			ca:          ca,
			targetPhase: services.RotationPhaseStandby,
			mode:        services.RotationModeAuto,
			gracePeriod: rotation.GracePeriod.Duration,
			schedule:    rotation.Schedule,
		}
	default:
		return trace.BadParameter("phase is not supported: %q", rotation.Phase)
	}
	logger.Infof("Setting rotation phase %q", req.targetPhase)
	rotated, err := processRotationRequest(*req)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := a.CompareAndSwapCertAuthority(rotated, ca); err != nil {
		return trace.Wrap(err)
	}
	logger.Infof("Cert authority rotation request is completed")
	return nil
}

// processRotationRequest processes rotation request FSM-style
// switches Phase and State
func processRotationRequest(req rotationReq) (services.CertAuthority, error) {
	rotation := req.ca.GetRotation()
	ca := req.ca.Clone()

	switch req.targetPhase {
	// this is the first stage of the rotation - new certificate authorities
	// are being generated, clients will start using new credentials
	// and servers will use existing credentials, but will trust clients
	// using new credentials
	case services.RotationPhaseUpdateClients:
		switch rotation.State {
		case services.RotationStateStandby, "":
		default:
			return nil, trace.BadParameter("can not initate rotation while another is in progress")
		}
		if err := startNewRotation(req, ca); err != nil {
			return nil, trace.Wrap(err)
		}
		return ca, nil
		// update server phase trusts uses the new credentials both for servers
		// and clients, but still trusts clients with old credentials
	case services.RotationPhaseUpdateServers:
		if rotation.Phase != services.RotationPhaseUpdateClients {
			return nil, trace.BadParameter(
				"can only switch to phase %v from %v, current phase is %v",
				services.RotationPhaseUpdateServers,
				services.RotationPhaseUpdateClients,
				rotation.Phase)
		}
		// this is simply update of the phase to signal nodes to restart
		// and start serving new signatures
		rotation.Phase = req.targetPhase
		rotation.Mode = req.mode
		ca.SetRotation(rotation)
		return ca, nil
		// rollback moves back both clients and servers to use old credentials
		// but will trust new credentials
	case services.RotationPhaseRollback:
		switch rotation.Phase {
		case services.RotationPhaseUpdateClients, services.RotationPhaseUpdateServers:
			if err := startRollingBackRotation(ca); err != nil {
				return nil, trace.Wrap(err)
			}
			return ca, nil
		}
		// this is to complete rotation, moves overall rotation
		// to standby, servers will only trust one CA
	case services.RotationPhaseStandby:
		switch rotation.Phase {
		case services.RotationPhaseUpdateServers, services.RotationPhaseRollback:
			if err := completeRotation(req.clock, ca); err != nil {
				return nil, trace.Wrap(err)
			}
			return ca, nil
		default:
			return nil, trace.BadParameter(
				"can only switch to phase %v from %v, current phase is %v",
				services.RotationPhaseUpdateServers,
				services.RotationPhaseUpdateClients,
				rotation.Phase)
		}
	default:
		return nil, trace.BadParameter("unsupported phase: %q", req.targetPhase)
	}
	return nil, trace.BadParameter("internal error")
}

// startNewRotation starts new rotation and in place updates the certificate
// authority with new CA keys
func startNewRotation(req rotationReq, ca services.CertAuthority) error {
	clock := req.clock
	gracePeriod := req.gracePeriod

	rotation := ca.GetRotation()
	id := uuid.New()

	rotation.Mode = req.mode
	rotation.Schedule = req.schedule

	// first part of the function generates credentials
	sshPrivPEM, sshPubPEM, err := native.GenerateKeyPair("")
	if err != nil {
		return trace.Wrap(err)
	}

	keyPEM, certPEM, err := tlsca.GenerateSelfSignedCA(pkix.Name{
		CommonName:   ca.GetClusterName(),
		Organization: []string{ca.GetClusterName()},
	}, nil, defaults.CATTL)
	if err != nil {
		return trace.Wrap(err)
	}
	tlsKeyPair := &services.TLSKeyPair{
		Cert: certPEM,
		Key:  keyPEM,
	}

	// second part of the function rotates the certificate authority
	rotation.Started = clock.Now().UTC()
	rotation.GracePeriod = services.NewDuration(gracePeriod)
	rotation.CurrentID = id

	signingKeys := ca.GetSigningKeys()
	checkingKeys := ca.GetCheckingKeys()
	keyPairs := ca.GetTLSKeyPairs()

	// drop old certificate authority without keeping it as trusted
	if gracePeriod == 0 {
		signingKeys = [][]byte{sshPrivPEM}
		checkingKeys = [][]byte{sshPubPEM}
		keyPairs = []services.TLSKeyPair{*tlsKeyPair}
		// in case of force rotation, rotation has been started and completed
		// in the same step moving it to standby state
		rotation.State = services.RotationStateStandby
	} else {
		// rotation sets the first key to be the new key
		// and keep only public keys/certs for the new CA
		signingKeys = [][]byte{sshPrivPEM, signingKeys[0]}
		checkingKeys = [][]byte{sshPubPEM, checkingKeys[0]}
		oldKeyPair := keyPairs[0]
		keyPairs = []services.TLSKeyPair{*tlsKeyPair, oldKeyPair}
		rotation.State = services.RotationStateInProgress
		rotation.Phase = services.RotationPhaseUpdateClients
	}

	ca.SetSigningKeys(signingKeys)
	ca.SetCheckingKeys(checkingKeys)
	ca.SetTLSKeyPairs(keyPairs)
	ca.SetRotation(rotation)
	return nil
}

// startRollingBackRotation starts rolls back rotation to the previous state
func startRollingBackRotation(ca services.CertAuthority) error {
	rotation := ca.GetRotation()

	// rollback always sets rotation to manual mode
	rotation.Mode = services.RotationModeManual

	// second part of the function rotates the certificate authority
	signingKeys := ca.GetSigningKeys()
	checkingKeys := ca.GetCheckingKeys()
	keyPairs := ca.GetTLSKeyPairs()

	// rotation sets the first key to be the new key
	// and keep only public keys/certs for the new CA
	signingKeys = [][]byte{signingKeys[1]}
	checkingKeys = [][]byte{checkingKeys[1]}

	// here, keep the attempted key pair certificate as trusted
	// as during rollback phases, both types of clients may be present in the cluster
	keyPairs = []services.TLSKeyPair{keyPairs[1], services.TLSKeyPair{Cert: keyPairs[0].Cert}}
	rotation.State = services.RotationStateInProgress
	rotation.Phase = services.RotationPhaseRollback

	ca.SetSigningKeys(signingKeys)
	ca.SetCheckingKeys(checkingKeys)
	ca.SetTLSKeyPairs(keyPairs)
	ca.SetRotation(rotation)
	return nil
}

// completeRollingBackRotation completes rollback of the rotation
// sets it to the standby state
func completeRollingBackRotation(clock clockwork.Clock, ca services.CertAuthority) error {
	rotation := ca.GetRotation()

	// clean up the state
	rotation.Started = time.Time{}
	rotation.State = services.RotationStateStandby
	rotation.Phase = services.RotationPhaseStandby
	rotation.Mode = ""
	rotation.Schedule = services.RotationSchedule{}

	keyPairs := ca.GetTLSKeyPairs()
	// only keep the original certificate authority as trusted
	// and remove all extra
	keyPairs = []services.TLSKeyPair{keyPairs[0]}

	ca.SetTLSKeyPairs(keyPairs)
	ca.SetRotation(rotation)
	return nil
}

// completeRotation completes certificate authority rotation
func completeRotation(clock clockwork.Clock, ca services.CertAuthority) error {
	rotation := ca.GetRotation()
	signingKeys := ca.GetSigningKeys()
	checkingKeys := ca.GetCheckingKeys()
	keyPairs := ca.GetTLSKeyPairs()

	signingKeys = signingKeys[:1]
	checkingKeys = checkingKeys[:1]
	keyPairs = keyPairs[:1]

	rotation.Started = time.Time{}
	rotation.State = services.RotationStateStandby
	rotation.Phase = services.RotationPhaseStandby
	rotation.LastRotated = clock.Now()
	rotation.Mode = ""
	rotation.Schedule = services.RotationSchedule{}

	ca.SetSigningKeys(signingKeys)
	ca.SetCheckingKeys(checkingKeys)
	ca.SetTLSKeyPairs(keyPairs)
	ca.SetRotation(rotation)
	return nil
}
