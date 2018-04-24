package service

import (
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
)

// connectToAuthService attempts to login into the auth servers specified in the
// configuration. Returns 'true' if successful
func (process *TeleportProcess) connectToAuthService(role teleport.Role) (*Connector, error) {
	connector, err := process.connect(role)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	process.addConnector(connector)
	return connector, nil
}

func (process *TeleportProcess) connect(role teleport.Role) (*Connector, error) {
	state, err := process.storage.GetState(role)
	if err != nil {
		if !trace.IsNotFound(err) {
			return nil, trace.Wrap(err)
		}
		// no state recorded means that this is the first connect
		// process haven't connected yet, so we expect the token to exist
		return process.firstTimeConnect(role)
	}

	identity, err := process.GetIdentity(role)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	rotation := state.Spec.Rotation

	switch rotation.State {
	// rotation is on standby, so just use whatever is current
	case "", services.RotationStateStandby:
		// admin and auth are a bit special, as it does not need clients
		if role == teleport.RoleAdmin || role == teleport.RoleAuth {
			return &Connector{
				ClientIdentity: identity,
				ServerIdentity: identity,
				AuthServer:     process.getLocalAuth(),
			}, nil
		}
		log.Infof("Connecting to the cluster %v with TLS client certificate.", identity.ClusterName)
		client, err := newClient(process.Config.AuthServers, identity)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return &Connector{Client: client, ClientIdentity: identity, ServerIdentity: identity}, nil
	case services.RotationStateInProgress:
		switch rotation.Phase {
		case services.RotationPhaseUpdateClients:
			// in this phase, clients should use updated credentials,
			// while servers should use old credentials to answer auth requests
			newIdentity, err := process.storage.ReadIdentity(auth.IdentityReplacement, role)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			if role == teleport.RoleAdmin || role == teleport.RoleAuth {
				return &Connector{
					ClientIdentity: newIdentity,
					ServerIdentity: identity,
					AuthServer:     process.getLocalAuth(),
				}, nil
			}
			client, err := newClient(process.Config.AuthServers, newIdentity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &Connector{
				Client:         client,
				ClientIdentity: newIdentity,
				ServerIdentity: identity,
			}, nil
		case services.RotationPhaseUpdateServers:
			// in this phase, servers and clients are using new identity, but the
			// identity is still set up to trust the old certificate authority certificates
			newIdentity, err := process.storage.ReadIdentity(auth.IdentityReplacement, role)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			if role == teleport.RoleAdmin || role == teleport.RoleAuth {
				return &Connector{
					ClientIdentity: newIdentity,
					ServerIdentity: newIdentity,
					AuthServer:     process.getLocalAuth(),
				}, nil
			}
			client, err := newClient(process.Config.AuthServers, newIdentity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &Connector{
				Client:         client,
				ClientIdentity: newIdentity,
				ServerIdentity: newIdentity,
			}, nil
		case services.RotationPhaseRollback:
			// in rollback phase, clients and servers should switch back
			// to the old certificate authority-issued credentials,
			// but new certificate authority should be trusted
			// because not all clients can update at the same time
			if role == teleport.RoleAdmin || role == teleport.RoleAuth {
				return &Connector{
					ClientIdentity: identity,
					ServerIdentity: identity,
					AuthServer:     process.getLocalAuth(),
				}, nil
			}
			client, err := newClient(process.Config.AuthServers, identity)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			return &Connector{
				Client:         client,
				ClientIdentity: identity,
				ServerIdentity: identity,
			}, nil
		default:
			return nil, trace.BadParameter("unsupported rotation phase: %q", rotation.Phase)
		}
	default:
		return nil, trace.BadParameter("unsupported rotation state: %q", rotation.State)
	}
}

func (process *TeleportProcess) firstTimeConnect(role teleport.Role) (*Connector, error) {
	id := auth.IdentityID{
		Role:     role,
		HostUUID: process.Config.HostUUID,
		NodeName: process.Config.Hostname,
	}
	additionalPrincipals, err := process.getAdditionalPrincipals(role)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var identity *auth.Identity
	if process.getLocalAuth() != nil {
		// Auth service is on the same host, no need to go though the invitation
		// procedure
		log.Debugf("This server has local Auth server started, using it to add role to the cluster.")
		identity, err = auth.LocalRegister(id, process.getLocalAuth(), additionalPrincipals)
	} else {
		// Auth server is remote, so we need a provisioning token
		if process.Config.Token == "" {
			return nil, trace.BadParameter("%v must join a cluster and needs a provisioning token", role)
		}
		log.Infof("Joining the cluster with a token %v.", process.Config.Token)
		identity, err = auth.Register(process.Config.DataDir, process.Config.Token, id, process.Config.AuthServers, additionalPrincipals)
	}
	if err != nil {
		return nil, trace.Wrap(err)
	}

	log.Infof("%v has successfully registered with the cluster.", role)
	var connector *Connector
	if role == teleport.RoleAdmin || role == teleport.RoleAuth {
		connector = &Connector{
			ClientIdentity: identity,
			ServerIdentity: identity,
			AuthServer:     process.getLocalAuth(),
		}
	} else {
		client, err := newClient(process.Config.AuthServers, identity)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		connector = &Connector{
			ClientIdentity: identity,
			ServerIdentity: identity,
			Client:         client,
		}
	}

	// sync local rotation state to match remote rotation state
	ca, err := connector.GetCertAuthority(services.CertAuthID{
		DomainName: connector.ClientIdentity.ClusterName,
		Type:       services.HostCA,
	}, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	err = process.storage.WriteIdentity(auth.IdentityCurrent, *identity)
	if err != nil {
		log.Warningf("%v failed to write identity: %v", err)
	}

	err = process.storage.WriteState(role, auth.StateV2{
		Spec: auth.StateSpecV2{
			Rotation: ca.GetRotation(),
		},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	log.Infof("%v has successfully wrote credentials and state to disk.", role)
	return connector, nil
}

// periodicSyncRotationState checks rotation state periodically and
// takes action if necessary
func (process *TeleportProcess) periodicSyncRotationState() error {
	t := time.NewTicker(defaults.HighResPollingPeriod)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			needsReload, err := process.syncRotationState()
			if err != nil {
				if trace.IsConnectionProblem(err) {
					log.Warningf("Connection problem: sync rotation state: %v", err)
				} else {
					log.Warningf("Failed to sync rotation state: %v", err)
				}
			} else if needsReload {
				process.BroadcastEvent(Event{Name: TeleportReloadEvent})
				return nil
			}
		case <-process.ExitContext().Done():
			log.Infof("Periodic rotation syncer has exited.")
			return nil
		}
	}
}

// syncRotationState compares cluster rotation state with local services state
// and performs rotation if necessary
func (process *TeleportProcess) syncRotationState() (bool, error) {
	var needsReload bool
	connectors := process.getConnectors()
	for _, conn := range connectors {
		reload, err := process.syncServiceRotationState(conn)
		if err != nil {
			return false, trace.Wrap(err)
		}
		if reload {
			needsReload = true
		}
	}
	return needsReload, nil
}

// syncServiceRotationState syncs up rotation state for individual service (Auth, Proxy, Node) and
// if necessary, updates credentials. Returns true if the service will need to reload.
func (process *TeleportProcess) syncServiceRotationState(conn *Connector) (bool, error) {
	state, err := process.storage.GetState(conn.ClientIdentity.ID.Role)
	if err != nil {
		return false, trace.Wrap(err)
	}
	ca, err := conn.GetCertAuthority(services.CertAuthID{
		DomainName: conn.ClientIdentity.ClusterName,
		Type:       services.HostCA,
	}, false)
	if err != nil {
		return false, trace.Wrap(err)
	}
	return process.rotate(conn, *state, ca.GetRotation())
}

// rotate is called to check if rotation should be triggered locally
func (process *TeleportProcess) rotate(conn *Connector, localState auth.StateV2, remote services.Rotation) (bool, error) {
	id := conn.ClientIdentity.ID
	local := localState.Spec.Rotation
	if local.Matches(remote) {
		// nothing to do, local state and rotation state are in sync
		return false, nil
	}

	additionalPrincipals, err := process.getAdditionalPrincipals(id.Role)
	if err != nil {
		return false, trace.Wrap(err)
	}

	storage := process.storage

	const outOfSync = "%v and cluster rotation state (%v) is out of sync with local (%v). Clear local state and re-register this %v."

	writeStateAndIdentity := func(name string, identity *auth.Identity) error {
		err = storage.WriteIdentity(name, *identity)
		if err != nil {
			return trace.Wrap(err)
		}
		localState.Spec.Rotation = remote
		err = storage.WriteState(id.Role, localState)
		if err != nil {
			return trace.Wrap(err)
		}
		return nil
	}

	// now, need to evaluate what is exact difference, there are
	// several supported scenarios, that this logic should handle
	switch remote.State {
	case "", services.RotationStateStandby:
		switch local.State {
		// great, nothing to do, it could happen
		// that the old node came up and missed the whole rotation
		// rollback cycle, but there is nothing we can do at this point
		case "", services.RotationStateStandby:
			if len(additionalPrincipals) != 0 && !conn.ServerIdentity.HasPrincipals(additionalPrincipals) {
				log.Infof("%v has updated principals to %q, going to request new principals and update")
				identity, err := conn.ReRegister(additionalPrincipals)
				if err != nil {
					return false, trace.Wrap(err)
				}
				err = storage.WriteIdentity(auth.IdentityCurrent, *identity)
				if err != nil {
					return false, trace.Wrap(err)
				}
				return true, nil
			}
			return false, nil
			// local rotation is in progress, if it has
			// just rolled back
		case services.RotationStateInProgress:
			// rollback phase has completed, all services
			// will receive new identities
			if local.Phase != services.RotationPhaseRollback && local.CurrentID != remote.CurrentID {
				return false, trace.CompareFailed(outOfSync, id.Role, remote, local, id.Role)
			}
			identity, err := conn.ReRegister(additionalPrincipals)
			if err != nil {
				return false, trace.Wrap(err)
			}
			err = writeStateAndIdentity(auth.IdentityCurrent, identity)
			if err != nil {
				return false, trace.Wrap(err)
			}
			return true, nil
		default:
			return false, trace.BadParameter("unsupported state: %q", localState)
		}
	case services.RotationStateInProgress:
		switch remote.Phase {
		case services.RotationPhaseStandby, "":
			// nothing to do
			return false, nil
		case services.RotationPhaseUpdateClients:
			// only allow transition in case if local rotation state is standby
			// so this server is in the "clean" state
			if local.State != services.RotationStateStandby && local.State != "" {
				return false, trace.CompareFailed(outOfSync, id.Role, remote, local, id.Role)
			}
			identity, err := conn.ReRegister(additionalPrincipals)
			if err != nil {
				return false, trace.Wrap(err)
			}
			err = writeStateAndIdentity(auth.IdentityReplacement, identity)
			if err != nil {
				return false, trace.Wrap(err)
			}
			// update of the servers and client requires reload of teleport process
			return true, nil
		case services.RotationPhaseUpdateServers:
			// allow transition to this phase only if the previous
			// phase was UpdateClients - as this is a happy scenario
			// when all phases are traversed in succession
			if local.Phase != services.RotationPhaseUpdateClients && local.CurrentID != remote.CurrentID {
				return false, trace.CompareFailed(outOfSync, id.Role, remote, local, id.Role)
			}
			// write replacement identity as a current identity and reload the server
			replacement, err := storage.ReadIdentity(auth.IdentityReplacement, id.Role)
			if err != nil {
				return false, trace.Wrap(err)
			}
			err = writeStateAndIdentity(auth.IdentityCurrent, replacement)
			if err != nil {
				return false, trace.Wrap(err)
			}
			// update of the servers requires reload of teleport process
			return true, nil
		case services.RotationPhaseRollback:
			// allow transition to this phase from any other local phase
			// because it will be widely used to recover cluster state to
			// the previously valid state
			// client will re-register to receive credentials signed by "old" CA
			identity, err := conn.ReRegister(additionalPrincipals)
			if err != nil {
				return false, trace.Wrap(err)
			}
			// update of the servers requires reload of teleport process
			err = writeStateAndIdentity(auth.IdentityCurrent, identity)
			if err != nil {
				return false, trace.Wrap(err)
			}
			return true, nil
		default:
			return false, trace.BadParameter("unsupported phase: %q", remote.Phase)
		}
	default:
		return false, trace.BadParameter("unsupported state: %q", remote.State)
	}
}

func newClient(authServers []utils.NetAddr, identity *auth.Identity) (*auth.Client, error) {
	tlsConfig, err := identity.TLSConfig()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return auth.NewTLSClient(authServers, tlsConfig)
}
