// Copyright 2020 MongoDB Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package atmcfg provides helpers for updating Ops Manager automation config.
package atmcfg // import "go.mongodb.org/ops-manager/atmcfg"

import (
	"crypto/sha1" //nolint:gosec // mongodb scram-sha-1 supports this tho is not recommended
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"go.mongodb.org/ops-manager/opsmngr"
	"go.mongodb.org/ops-manager/search"
)

func setDisabledByClusterName(out *opsmngr.AutomationConfig, name string, disabled bool) {
	// This value may not be present and is mandatory
	if out.Auth.DeploymentAuthMechanisms == nil {
		out.Auth.DeploymentAuthMechanisms = make([]string, 0)
	}
	setDisabledByReplicaSetName(out, name, disabled)
	setDisabledByShardName(out, name, disabled)
}

func setDisabledByReplicaSetName(out *opsmngr.AutomationConfig, name string, disabled bool) {
	i, found := search.ReplicaSets(out.ReplicaSets, func(rs *opsmngr.ReplicaSet) bool {
		return rs.ID == name
	})
	if found {
		rs := out.ReplicaSets[i]
		for _, m := range rs.Members {
			for k, p := range out.Processes {
				if p.Name == m.Host {
					out.Processes[k].Disabled = disabled
				}
			}
		}
	}
}

func setDisabledByShardName(out *opsmngr.AutomationConfig, name string, disabled bool) {
	i, found := search.ShardingConfig(out.Sharding, func(s *opsmngr.ShardingConfig) bool {
		return s.Name == name
	})
	if found {
		s := out.Sharding[i]
		for _, rs := range s.Shards {
			setDisabledByReplicaSetName(out, rs.ID, disabled)
		}
	}
}

// Shutdown disables all processes of the given cluster name
func Shutdown(out *opsmngr.AutomationConfig, name string) {
	setDisabledByClusterName(out, name, true)
}

// Startup enables all processes of the given cluster name
func Startup(out *opsmngr.AutomationConfig, name string) {
	setDisabledByClusterName(out, name, false)
}

// RemoveByClusterName removes a cluster and its associated processes from the config.
// This won't shutdown any running process.
func RemoveByClusterName(out *opsmngr.AutomationConfig, name string) {
	// This value may not be present and is mandatory
	if out.Auth.DeploymentAuthMechanisms == nil {
		out.Auth.DeploymentAuthMechanisms = make([]string, 0)
	}
	removeByReplicaSetName(out, name)
	removeByShardName(out, name)
}

func removeByReplicaSetName(out *opsmngr.AutomationConfig, name string) {
	i, found := search.ReplicaSets(out.ReplicaSets, func(rs *opsmngr.ReplicaSet) bool {
		return rs.ID == name
	})
	if found {
		rs := out.ReplicaSets[i]
		out.ReplicaSets = append(out.ReplicaSets[:i], out.ReplicaSets[i+1:]...)
		for _, m := range rs.Members {
			for k, p := range out.Processes {
				if p.Name == m.Host {
					out.Processes = append(out.Processes[:k], out.Processes[k+1:]...)
				}
			}
		}
	}
}

func removeByShardName(out *opsmngr.AutomationConfig, name string) {
	i, found := search.ShardingConfig(out.Sharding, func(rs *opsmngr.ShardingConfig) bool {
		return rs.Name == name
	})
	if found {
		s := out.Sharding[i]
		out.Sharding = append(out.Sharding[:i], out.Sharding[i+1:]...)
		for _, rs := range s.Shards {
			removeByReplicaSetName(out, rs.ID)
		}
	}
}

// AddUser adds a opsmngr.MongoDBUser to the opsmngr.AutomationConfi
func AddUser(out *opsmngr.AutomationConfig, u *opsmngr.MongoDBUser) {
	out.Auth.Users = append(out.Auth.Users, u)
}

// ConfigureScramCredentials creates both SCRAM-SHA-1 and SCRAM-SHA-256 credentials.
// Use this method to guarantee that password can be updated later.
func ConfigureScramCredentials(user *opsmngr.MongoDBUser, password string) error {
	scram256Creds, err := newScramSha256Creds(user, password)
	if err != nil {
		return err
	}

	scram1Creds, err := newScramSha1Creds(user, password)
	if err != nil {
		return err
	}
	user.ScramSha256Creds = scram256Creds
	user.ScramSha1Creds = scram1Creds
	return nil
}

func newScramSha1Creds(user *opsmngr.MongoDBUser, password string) (*opsmngr.ScramShaCreds, error) {
	scram1Salt, err := generateSalt(sha1.New)
	if err != nil {
		return nil, fmt.Errorf("error generating scramSha1 salt: %s", err)
	}
	scram1Creds, err := newScramShaCreds(scram1Salt, user.Username, password, mongoCR)
	if err != nil {
		return nil, fmt.Errorf("error generating scramSha1Creds: %s", err)
	}
	return scram1Creds, nil
}

func newScramSha256Creds(user *opsmngr.MongoDBUser, password string) (*opsmngr.ScramShaCreds, error) {
	scram256Salt, err := generateSalt(sha256.New)
	if err != nil {
		return nil, fmt.Errorf("error generating scramSha256 salt: %s", err)
	}
	scram256Creds, err := newScramShaCreds(scram256Salt, user.Username, password, scramSha256)
	if err != nil {
		return nil, fmt.Errorf("error generating scramSha256 creds: %s", err)
	}
	return scram256Creds, nil
}

// newScramShaCreds takes a plain text password and a specified mechanism name and generates
// the ScramShaCreds which will be embedded into a MongoDBUser.
func newScramShaCreds(salt []byte, username, password, mechanism string) (*opsmngr.ScramShaCreds, error) {
	if mechanism != scramSha256 && mechanism != mongoCR {
		return nil, fmt.Errorf("unrecognized SCRAM-SHA format %s", mechanism)
	}
	var hashConstructor hashingFunc
	iterations := 0
	if mechanism == scramSha256 {
		hashConstructor = sha256.New
		iterations = scramSha256Iterations
	} else if mechanism == mongoCR {
		hashConstructor = sha1.New
		iterations = scramSha1Iterations

		// MONGODB-CR/SCRAM-SHA-1 requires the hash of the password being passed computeScramCredentials
		// instead of the plain text password.
		var err error
		password, err = md5Hex(username + ":mongo:" + password)
		if err != nil {
			return nil, err
		}
	}
	base64EncodedSalt := base64.StdEncoding.EncodeToString(salt)
	return computeScramCredentials(hashConstructor, iterations, base64EncodedSalt, password)
}

// AddIndexConfig adds an opsmngr.IndexConfig to the opsmngr.AutomationConfig
func AddIndexConfig(out *opsmngr.AutomationConfig, newIndex *opsmngr.IndexConfig) error {
	if out == nil {
		return errors.New("the Automation Config has not been initialized")
	}
	_, exists := search.MongoDBIndexes(out.IndexConfigs, compareIndexConfig(newIndex))

	if exists {
		return errors.New("index already exists")
	}
	out.IndexConfigs = append(out.IndexConfigs, newIndex)

	return nil
}

// compareIndexConfig returns a function that compares two indexConfig struts
func compareIndexConfig(newIndex *opsmngr.IndexConfig) func(index *opsmngr.IndexConfig) bool {
	return func(index *opsmngr.IndexConfig) bool {
		if newIndex.RSName == index.RSName && newIndex.CollectionName == index.CollectionName && newIndex.DBName == index.DBName && len(newIndex.Key) == len(index.Key) {
			// if keys are equal the two indexes are considered to be the same
			for i := 0; i < len(newIndex.Key); i++ {
				if newIndex.Key[i][0] != index.Key[i][0] || newIndex.Key[i][1] != index.Key[i][1] {
					return false
				}
			}
			return true
		}
		return false
	}
}

// RemoveUser removes a MongoDBUser from the authentication config
func RemoveUser(out *opsmngr.AutomationConfig, username, database string) error {
	pos, found := search.MongoDBUsers(out.Auth.Users, func(p *opsmngr.MongoDBUser) bool {
		return p.Username == username && p.Database == database
	})
	if !found {
		return fmt.Errorf("user '%s' not found for '%s'", username, database)
	}
	out.Auth.Users = append(out.Auth.Users[:pos], out.Auth.Users[pos+1:]...)
	return nil
}

const (
	automationAgentName            = "mms-automation"
	keyLength                      = 500
	mongoCR                        = "MONGODB-CR"
	scramSha256                    = "SCRAM-SHA-256"
	atmAgentWindowsKeyFilePath     = "%SystemDrive%\\MMSAutomation\\versions\\keyfile"
	atmAgentKeyFilePathInContainer = "/var/lib/mongodb-mms-automation/keyfile"
)

// EnableMechanism allows you to enable a given set of authentication mechanisms to an opsmngr.AutomationConfig.
// This method currently only supports MONGODB-CR, and SCRAM-SHA-256
func EnableMechanism(out *opsmngr.AutomationConfig, m []string) error {
	out.Auth.Disabled = false
	for _, v := range m {
		if v != mongoCR && v != scramSha256 {
			return fmt.Errorf("unsupported mechanism %s", v)
		}
		if v == scramSha256 && out.Auth.AutoAuthMechanism == "" {
			out.Auth.AutoAuthMechanism = v
		}
		if !stringInSlice(out.Auth.DeploymentAuthMechanisms, v) {
			out.Auth.DeploymentAuthMechanisms = append(out.Auth.DeploymentAuthMechanisms, v)
		}
		if !stringInSlice(out.Auth.AutoAuthMechanisms, v) {
			out.Auth.AutoAuthMechanisms = append(out.Auth.AutoAuthMechanisms, v)
		}
	}

	if out.Auth.AutoUser == "" && out.Auth.AutoPwd == "" {
		if err := setAutoUser(out); err != nil {
			return err
		}
	}

	if out.Auth.Key == "" {
		var err error
		if out.Auth.Key, err = generateRandomBase64String(keyLength); err != nil {
			return err
		}
	}
	if out.Auth.KeyFile == "" {
		out.Auth.KeyFile = atmAgentKeyFilePathInContainer
	}
	if out.Auth.KeyFileWindows == "" {
		out.Auth.KeyFileWindows = atmAgentWindowsKeyFilePath
	}

	return nil
}

func setAutoUser(out *opsmngr.AutomationConfig) error {
	var err error
	out.Auth.AutoUser = automationAgentName
	if out.Auth.AutoPwd, err = generateRandomASCIIString(keyLength); err != nil {
		return err
	}

	return nil
}

func stringInSlice(a []string, x string) bool {
	for _, b := range a {
		if b == x {
			return true
		}
	}
	return false
}
