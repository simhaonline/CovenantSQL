package raft

import (
	"fmt"
	"github.com/thunderdb/ThunderDB/crypto/signature"
)

// ServerSuffrage determines whether a Server in a Configuration gets a vote.
type ServerSuffrage int

// Note: Don't renumber these, since the numbers are written into the log.
const (
	// Peer is a server whose vote is counted in elections and whose match index
	// is used in advancing the leader's commit index.
	Peer ServerSuffrage = iota
	// Learner is a server that receives log entries but is not considered for
	// elections or commitment purposes.
	Learner
	// Staging is a server that acts like a nonvoter with one exception: once a
	// staging server receives enough log entries to be sufficiently caught up to
	// the leader's log, the leader will invoke a  membership change to change
	// the Staging server to a Peer.
	Staging
)

func (s ServerSuffrage) String() string {
	switch s {
	case Peer:
		return "Peer"
	case Learner:
		return "Learner"
	case Staging:
		return "Staging"
	}
	return "ServerSuffrage"
}

// ServerID is a unique string identifying a server for all time.
type ServerID string

// ServerAddress is a network address for a server that a transport can contact.
type ServerAddress string

// Server tracks the information about a single server in a configuration.
type Server struct {
	// Suffrage determines whether the server gets a vote.
	Suffrage ServerSuffrage
	// ID is a unique string identifying this server for all time.
	ID ServerID
	// Address is its network address that a transport can contact.
	Address ServerAddress
	// Public key
	PubKey *signature.PublicKey
}

// Configuration tracks which servers are in the cluster, and whether they have
// votes. This should include the local server, if it's a member of the cluster.
// The servers are listed no particular order, but each should only appear once.
// These entries are appended to the log during membership changes.
type Configuration struct {
	Term      uint64
	Leader    Server
	Servers   []Server
	Signature *signature.Signature
}

// Clone makes a deep copy of a Configuration.
func (c *Configuration) Clone() (copy Configuration) {
	copy.Term = c.Term
	copy.Leader = c.Leader
	copy.Servers = append(copy.Servers, c.Servers...)
	copy.Signature = c.Signature
	return
}

// ConfigurationChangeCommand is the different ways to change the cluster
// configuration.
type ConfigurationChangeCommand uint8

const (
	// AddStaging makes a server Staging unless its Peer.
	AddStaging ConfigurationChangeCommand = iota
	// AddLearner makes a server Learner unless its Staging or Peer.
	AddLearner
	// Demote makes a server Learner unless its absent.
	Demote
	// RemoveServer removes a server entirely from the cluster membership.
	RemoveServer
	// Promote is created automatically by a leader; it turns a Staging server
	// into a Peer.
	Promote
)

func (c ConfigurationChangeCommand) String() string {
	switch c {
	case AddStaging:
		return "AddStaging"
	case AddLearner:
		return "AddLearner"
	case Demote:
		return "Demote"
	case RemoveServer:
		return "RemoveServer"
	case Promote:
		return "Promote"
	}
	return "ConfigurationChangeCommand"
}

// configurationChangeRequest describes a change that a leader would like to
// make to its current configuration. It's used only within a single server
// (never serialized into the log), as part of `configurationChangeFuture`.
type configurationChangeRequest struct {
	command       ConfigurationChangeCommand
	serverID      ServerID
	serverAddress ServerAddress // only present for AddStaging, AddLearner
	// prevIndex, if nonzero, is the index of the only configuration upon which
	// this change may be applied; if another configuration entry has been
	// added in the meantime, this request will fail.
	prevIndex uint64
}

// configurations is state tracked on every server about its Configurations.
// Note that, per Diego's dissertation, there can be at most one uncommitted
// configuration at a time (the next configuration may not be created until the
// prior one has been committed).
//
// One downside to storing just two configurations is that if you try to take a
// snahpsot when your state machine hasn't yet applied the committedIndex, we
// have no record of the configuration that would logically fit into that
// snapshot. We disallow snapshots in that case now. An alternative approach,
// which LogCabin uses, is to track every configuration change in the
// log.
type configurations struct {
	// committed is the latest configuration in the log/snapshot that has been
	// committed (the one with the largest index).
	committed Configuration
	// committedIndex is the log index where 'committed' was written.
	committedIndex uint64
	// latest is the latest configuration in the log/snapshot (may be committed
	// or uncommitted)
	latest Configuration
	// latestIndex is the log index where 'latest' was written.
	latestIndex uint64
}

// Clone makes a deep copy of a configurations object.
func (c *configurations) Clone() (copy configurations) {
	copy.committed = c.committed.Clone()
	copy.committedIndex = c.committedIndex
	copy.latest = c.latest.Clone()
	copy.latestIndex = c.latestIndex
	return
}

// isPeer returns true if the server identified by 'id' is a Peer in the
// provided Configuration.
func isPeer(configuration Configuration, id ServerID) bool {
	for _, server := range configuration.Servers {
		if server.ID == id {
			return server.Suffrage == Peer
		}
	}
	return false
}

// checkConfiguration tests a cluster membership configuration for common
// errors.
func checkConfiguration(configuration Configuration) error {
	idSet := make(map[ServerID]bool)
	addressSet := make(map[ServerAddress]bool)
	var voters int
	for _, server := range configuration.Servers {
		if server.ID == "" {
			return fmt.Errorf("Empty ID in configuration: %v", configuration)
		}
		if server.Address == "" {
			return fmt.Errorf("Empty address in configuration: %v", server)
		}
		if idSet[server.ID] {
			return fmt.Errorf("Found duplicate ID in configuration: %v", server.ID)
		}
		idSet[server.ID] = true
		if addressSet[server.Address] {
			return fmt.Errorf("Found duplicate address in configuration: %v", server.Address)
		}
		addressSet[server.Address] = true
		if server.Suffrage == Peer {
			voters++
		}
	}
	if voters == 0 {
		return fmt.Errorf("Need at least one voter in configuration: %v", configuration)
	}
	return nil
}

// nextConfiguration generates a new Configuration from the current one and a
// configuration change request. It's split from appendConfigurationEntry so
// that it can be unit tested easily.
func nextConfiguration(current Configuration, currentIndex uint64, change configurationChangeRequest) (Configuration, error) {
	if change.prevIndex > 0 && change.prevIndex != currentIndex {
		return Configuration{}, fmt.Errorf("Configuration changed since %v (latest is %v)", change.prevIndex, currentIndex)
	}

	configuration := current.Clone()
	switch change.command {
	case AddStaging:
		// TODO: barf on new address?
		newServer := Server{
			// TODO: This should add the server as Staging, to be automatically
			// promoted to Peer later. However, the promoton to Peer is not yet
			// implemented, and doing so is not trivial with the way the leader loop
			// coordinates with the replication goroutines today. So, for now, the
			// server will have a vote right away, and the Promote case below is
			// unused.
			Suffrage: Peer,
			ID:       change.serverID,
			Address:  change.serverAddress,
		}
		found := false
		for i, server := range configuration.Servers {
			if server.ID == change.serverID {
				if server.Suffrage == Peer {
					configuration.Servers[i].Address = change.serverAddress
				} else {
					configuration.Servers[i] = newServer
				}
				found = true
				break
			}
		}
		if !found {
			configuration.Servers = append(configuration.Servers, newServer)
		}
	case AddLearner:
		newServer := Server{
			Suffrage: Learner,
			ID:       change.serverID,
			Address:  change.serverAddress,
		}
		found := false
		for i, server := range configuration.Servers {
			if server.ID == change.serverID {
				if server.Suffrage != Learner {
					configuration.Servers[i].Address = change.serverAddress
				} else {
					configuration.Servers[i] = newServer
				}
				found = true
				break
			}
		}
		if !found {
			configuration.Servers = append(configuration.Servers, newServer)
		}
	case Demote:
		for i, server := range configuration.Servers {
			if server.ID == change.serverID {
				configuration.Servers[i].Suffrage = Learner
				break
			}
		}
	case RemoveServer:
		for i, server := range configuration.Servers {
			if server.ID == change.serverID {
				configuration.Servers = append(configuration.Servers[:i], configuration.Servers[i+1:]...)
				break
			}
		}
	case Promote:
		for i, server := range configuration.Servers {
			if server.ID == change.serverID && server.Suffrage == Staging {
				configuration.Servers[i].Suffrage = Peer
				break
			}
		}
	}

	// Make sure we didn't do something bad like remove the last voter
	if err := checkConfiguration(configuration); err != nil {
		return Configuration{}, err
	}

	return configuration, nil
}

// encodePeers is used to serialize a Configuration into the old peers format.
// This is here for backwards compatibility when operating with a mix of old
// servers and should be removed once we deprecate support for protocol version 1.
func encodePeers(configuration Configuration, trans Transport) []byte {
	// Gather up all the voters, other suffrage types are not supported by
	// this data format.
	var encPeers [][]byte
	for _, server := range configuration.Servers {
		if server.Suffrage == Peer {
			encPeers = append(encPeers, trans.EncodePeer(server.ID, server.Address))
		}
	}

	// Encode the entire array.
	buf, err := encodeMsgPack(encPeers)
	if err != nil {
		panic(fmt.Errorf("failed to encode peers: %v", err))
	}

	return buf.Bytes()
}

// decodePeers is used to deserialize an old list of peers into a Configuration.
// This is here for backwards compatibility with old log entries and snapshots;
// it should be removed eventually.
func decodePeers(buf []byte, trans Transport) Configuration {
	// Decode the buffer first.
	var encPeers [][]byte
	if err := decodeMsgPack(buf, &encPeers); err != nil {
		panic(fmt.Errorf("failed to decode peers: %v", err))
	}

	// Deserialize each peer.
	var servers []Server
	for _, enc := range encPeers {
		p := trans.DecodePeer(enc)
		servers = append(servers, Server{
			Suffrage: Peer,
			ID:       ServerID(p),
			Address:  ServerAddress(p),
		})
	}

	return Configuration{
		Servers: servers,
	}
}

// encodeConfiguration serializes a Configuration using MsgPack, or panics on
// errors.
func encodeConfiguration(configuration Configuration) []byte {
	buf, err := encodeMsgPack(configuration)
	if err != nil {
		panic(fmt.Errorf("failed to encode configuration: %v", err))
	}
	return buf.Bytes()
}

// decodeConfiguration deserializes a Configuration using MsgPack, or panics on
// errors.
func decodeConfiguration(buf []byte) Configuration {
	var configuration Configuration
	if err := decodeMsgPack(buf, &configuration); err != nil {
		panic(fmt.Errorf("failed to decode configuration: %v", err))
	}
	return configuration
}
