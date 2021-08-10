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

package opsservice

import (
	"encoding/json"
	"strings"

	"github.com/gravitational/gravity/lib/storage/clusterconfig"

	"github.com/gravitational/gravity/lib/loc"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/utils"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
)

// rotateSecrets generates a new set of TLS keys for the given node
// as a package that will be automatically downloaded during upgrade
func (s *site) rotateSecrets(ctx *operationContext, secretsPackage loc.Locator,
	node *ProvisionedServer, serviceCIDR string, config []byte) (*ops.RotatePackageResponse, error) {
	if !node.IsMaster() {
		resp, err := s.getPlanetNodeSecretsPackage(ctx, node, secretsPackage)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		return resp, nil
	}

	var clusterConfig *clusterconfig.Resource
	if len(config) != 0 {
		log.WithField("requestConfig", string(config)).Info("Cluster configuration.6")
		var err error
		clusterConfig, err = clusterconfig.Unmarshal(config)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if configOut, err := json.Marshal(clusterConfig); err == nil {
			log.WithField("config", string(configOut)).Info("Cluster configuration.7")
		}
	} else {
		var err error
		clusterConfig, err = s.getClusterConfiguration()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if configOut, err := json.Marshal(clusterConfig); err == nil {
			log.WithField("config", string(configOut)).Info("Cluster configuration.8")
		}
	}

	masterParams := planetMasterParams{
		master:            node,
		secretsPackage:    &secretsPackage,
		serviceSubnet:     serviceCIDR,
		apiServerCertSANs: clusterConfig.Spec.Global.APIServerCertSANs,
	}
	// if we have a connection to Ops Center set up, configure
	// SNI host so Ops Center can dial in
	trustedCluster, err := storage.GetTrustedCluster(s.backend())
	if err == nil {
		certSAN := strings.Join([]string{s.domainName, trustedCluster.GetSNIHost()}, ".")
		masterParams.apiServerCertSANs = utils.AppendIfMissing(masterParams.apiServerCertSANs, certSAN)
	}

	resp, err := s.getPlanetMasterSecretsPackage(ctx, masterParams)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return resp, nil
}
