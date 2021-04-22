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
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gravitational/gravity/lib/clients"
	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/httplib"
	"github.com/gravitational/gravity/lib/kubernetes"
	"github.com/gravitational/gravity/lib/loc"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/pack"
	"github.com/gravitational/gravity/lib/schema"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/users"
	"github.com/gravitational/gravity/lib/utils"

	"github.com/cenkalti/backoff"
	teleservices "github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/trace"
	"github.com/pborman/uuid"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
)

// createShrinkOperation initiates shrink operation and starts it immediately
func (s *site) createShrinkOperation(context context.Context, req ops.CreateSiteShrinkOperationRequest) (*ops.SiteOperationKey, error) {
	log.Infof("createShrinkOperation: req=%#v", req)

	cluster, err := s.service.GetSite(s.key)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	server, err := s.validateShrinkRequest(req, *cluster)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	op := &ops.SiteOperation{
		ID:          uuid.New(),
		AccountID:   s.key.AccountID,
		SiteDomain:  s.key.SiteDomain,
		Type:        ops.OperationShrink,
		Created:     s.clock().UtcNow(),
		CreatedBy:   storage.UserFromContext(context),
		Updated:     s.clock().UtcNow(),
		State:       ops.OperationStateShrinkInProgress,
		Provisioner: server.Provisioner,
	}

	ctx, err := s.newOperationContext(*op)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer ctx.Close()

	err = s.updateRequestVars(ctx, &req.Variables, op)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	err = s.service.setCloudProviderFromRequest(s.key, op.Provisioner, &req.Variables)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	op.Shrink = &storage.ShrinkOperationState{
		Servers:     []storage.Server{*server},
		Force:       req.Force,
		Vars:        req.Variables,
		NodeRemoved: req.NodeRemoved,
	}
	op.Shrink.Vars.System.ClusterName = s.key.SiteDomain

	// make sure the provided keys are valid
	if isAWSProvisioner(op.Provisioner) {
		// when shrinking via command line (using leave/remove), AWS credentials are not
		// provided so skip their validation - terraform will retrieve the keys from AWS
		// metadata API automatically
		aws := s.cloudProvider().(*aws)
		if aws.accessKey != "" || aws.secretKey != "" {
			err = s.verifyPermissionsAWS(ctx)
			if err != nil {
				return nil, trace.BadParameter("invalid AWS credentials")
			}
		}
	}

	key, err := s.getOperationGroup().createSiteOperationWithOptions(*op,
		createOperationOptions{force: req.Force})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	s.reportProgress(ctx, ops.ProgressEntry{
		State:      ops.ProgressStateInProgress,
		Completion: 0,
		Message:    "initializing the operation",
	})

	err = s.executeOperation(*key, s.shrinkOperationStart)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return key, nil
}

func (s *site) validateShrinkRequest(req ops.CreateSiteShrinkOperationRequest, cluster ops.Site) (*storage.Server, error) {
	serverName := req.Servers[0]
	if len(cluster.ClusterState.Servers) == 1 {
		return nil, trace.BadParameter(
			"cannot shrink 1-node cluster, use --force flag to uninstall")
	}

	server, err := cluster.ClusterState.FindServer(serverName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// check to make sure the server exists and can be found
	servers, err := s.getAllTeleportServers()
	if err != nil {
		return nil, trace.Wrap(err, "failed to query teleport servers")
	}

	masters := servers.getWithLabels(labels{schema.ServiceLabelRole: string(schema.ServiceRoleMaster)})
	if len(masters) == 0 {
		return nil, trace.NotFound("no master servers found")
	}
	if len(masters) == 1 && masters[0].GetLabels()[ops.Hostname] == server.Hostname {
		return nil, trace.BadParameter("cannot remove the last master server")
	}

	teleserver := servers.getWithLabels(labels{ops.Hostname: server.Hostname})
	if len(teleserver) == 0 {
		if !req.Force {
			return nil, trace.BadParameter(
				"node %q is offline, add --force flag to force removal", serverName)
		}
		log.Warnf("Node %q is offline, forcing removal.", serverName)
	}

	return server, nil
}

// shrinkOperationStart kicks off actuall uninstall process:
// deprovisions servers, deletes packages
func (s *site) shrinkOperationStart(opCtx *operationContext) (err error) {
	state := opCtx.operation.Shrink
	opCtx.serversToRemove = state.Servers
	force := state.Force
	opKey := opCtx.key()
	serverName := state.Servers[0].Hostname
	logger := opCtx.WithField("server", serverName)

	cluster, err := s.service.GetSite(s.key)
	if err != nil {
		return trace.Wrap(err)
	}

	server, err := cluster.ClusterState.FindServer(serverName)
	if err != nil {
		return trace.Wrap(err)
	}

	// if the node is the gravity site leader (i.e. the process that is executing this code)
	// is running on is being removed, give up the leadership so another process will pick up
	// and resume the operation
	if server.AdvertiseIP == os.Getenv(constants.EnvPodIP) {
		opCtx.RecordInfo("this node is being removed, stepping down")
		s.leader().StepDown()
		return nil
	}

	// if the operation was resumed, cloud provider might not be set
	if s.service.getCloudProvider(s.key) == nil {
		err = s.service.setCloudProviderFromRequest(
			s.key, opCtx.operation.Provisioner, &opCtx.operation.Shrink.Vars)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	if force {
		opCtx.RecordInfo("forcing %q removal", serverName)
	} else {
		opCtx.RecordInfo("starting %q removal", serverName)
	}

	// schedule some clean up actions to run regardless of the outcome of the operation
	defer func() {
		// erase cloud provider info for this site which may contain sensitive information
		// such as API keys
		s.service.deleteCloudProvider(s.key)
		ctx, cancel := context.WithTimeout(context.Background(), defaults.AgentStopTimeout)
		defer cancel()
		err := s.agentService().StopAgents(ctx, opKey)
		if err != nil {
			logger.WithError(err).Warn("Failed to stop shrink agent.")
		}
	}()

	// shrink uses a couple of runners for the following purposes:
	//  * teleport master runner is used to execute system commands that remove
	//    the node from k8s, database, etc.
	//  * agent runner runs on the removed node and is used to perform system
	//    uninstall on it (if the node is online)
	var masterRunner, agentRunner *serverRunner

	masterRunner, err = s.pickShrinkMasterRunner(opCtx, *server)
	if err != nil {
		return trace.Wrap(err)
	}
	logger.Infof("Selected %v (%v) as master runner.",
		masterRunner.server.HostName(),
		masterRunner.server.Address())

	// determine whether the node being removed is online and, if so, launch
	// a shrink agent on it
	online := false
	if !state.NodeRemoved {
		_, err := s.getTeleportServerNoRetry(ops.Hostname, serverName)
		if err != nil {
			logger.WithError(err).Warn("Node is offline.")
		} else {
			logger.Info("Launch shrink agent.")
			agentRunner, err = s.launchAgent(context.TODO(), opCtx, *server)
			if err != nil {
				if !force {
					return trace.Wrap(err)
				}
				logger.WithError(err).Warn("Failed to launch agent.")
			} else {
				online = true
			}
		}
	}

	if online {
		opCtx.RecordInfo("node %q is online", serverName)
	} else {
		opCtx.RecordInfo("node %q is offline", serverName)
	}

	s.reportProgress(opCtx, ops.ProgressEntry{
		State:      ops.ProgressStateInProgress,
		Completion: 10,
		Message:    "unregistering the node",
	})

	if err = s.unlabelNode(*server, masterRunner); err != nil {
		if !force {
			return trace.Wrap(err, "failed to unregister the node")
		}
		logger.WithError(err).Warn("Failed to unregister the node, force continue.")
	}

	s.reportProgress(opCtx, ops.ProgressEntry{
		State:      ops.ProgressStateInProgress,
		Completion: 15,
		Message:    "disable planet elections",
	})

	if err = s.setElectionStatus(*server, false, masterRunner); err != nil {
		if !force {
			return trace.Wrap(err, "failed to disable elections on the node")
		}

		logger.WithError(err).Warn("Failed to disable elections on the node, force continue.")
	}

	if s.app.Manifest.HasHook(schema.HookNodeRemoving) {
		s.reportProgress(opCtx, ops.ProgressEntry{
			State:      ops.ProgressStateInProgress,
			Completion: 20,
			Message:    "running pre-removal hooks",
		})

		if err = s.runHook(opCtx, schema.HookNodeRemoving); err != nil {
			if !force {
				return trace.Wrap(err, "failed to run %v hook", schema.HookNodeRemoving)
			}
			logger.WithError(err).WithField("hook", schema.HookNodeRemoving).Warn("Failed to run hook, force continue.")
		}
	}

	// if the node is online, drain the node
	if online {
		if !force {
			s.reportProgress(opCtx, ops.ProgressEntry{
				State:      ops.ProgressStateInProgress,
				Completion: 30,
				Message:    "draining the node",
			})

			err = s.drain(*server)
			if err != nil {
				return trace.Wrap(err, "failed to drain the node")
			}
		}
	}

	s.reportProgress(opCtx, ops.ProgressEntry{
		State:      ops.ProgressStateInProgress,
		Completion: 40,
		Message:    "removing the node from the kubernetes cluster",
	})

	// delete the Kubernetes node
	if err = s.removeNodeFromCluster(*server, masterRunner); err != nil {
		if !force {
			return trace.Wrap(err, "failed to remove the node from the cluster")
		}
		logger.WithError(err).Warn("Failed to remove node from the cluster, force continue.")
	}

	s.reportProgress(opCtx, ops.ProgressEntry{
		State:      ops.ProgressStateInProgress,
		Completion: 45,
		Message:    "removing the node from the database",
	})

	// remove etcd member
	err = s.removeFromEtcd(context.TODO(), opCtx, *server)
	// the node may be an etcd proxy and not a full member of the etcd cluster
	if err != nil && !trace.IsNotFound(err) {
		if !force {
			return trace.Wrap(err, "failed to remove the node from the database")
		}
		logger.WithError(err).Warn("Failed to remove the node from the database, force continue.")
	}

	if online {
		s.reportProgress(opCtx, ops.ProgressEntry{
			State:      ops.ProgressStateInProgress,
			Completion: 50,
			Message:    "uninstalling the system software",
		})

		if err = s.uninstallSystem(opCtx, agentRunner); err != nil {
			logger.WithError(err).Warn("Failed to uninstall the system software.")
		}
	}

	if isAWSProvisioner(opCtx.operation.Provisioner) {
		if !s.app.Manifest.HasHook(schema.HookNodesDeprovision) {
			return trace.BadParameter("%v hook is not defined",
				schema.HookNodesDeprovision)
		}
		logger.Info("Using nodes deprovisioning hook.")
		err := s.runNodesDeprovisionHook(opCtx)
		if err != nil {
			return trace.Wrap(err)
		}
		opCtx.RecordInfo("nodes have been successfully deprovisioned")
	}

	if s.app.Manifest.HasHook(schema.HookNodeRemoved) {
		s.reportProgress(opCtx, ops.ProgressEntry{
			State:      ops.ProgressStateInProgress,
			Completion: 80,
			Message:    "running post-removal hooks",
		})

		if err = s.runHook(opCtx, schema.HookNodeRemoved); err != nil {
			if !force {
				return trace.Wrap(err, "failed to run %v hook", schema.HookNodeRemoved)
			}
			logger.WithError(err).WithField("hook", schema.HookNodeRemoved).Warn("Failed to run hook, force continue.")
		}
	}

	s.reportProgress(opCtx, ops.ProgressEntry{
		State:      ops.ProgressStateInProgress,
		Completion: 85,
		Message:    "cleaning up packages",
	})

	provisionedServer := &ProvisionedServer{Server: *server}
	if err = s.deletePackages(provisionedServer); err != nil {
		if !force {
			return trace.Wrap(err, "failed to clean up packages")
		}
		logger.WithError(err).Warn("Failed to clean up packages, force continue.")
	}

	s.reportProgress(opCtx, ops.ProgressEntry{
		State:      ops.ProgressStateInProgress,
		Completion: 90,
		Message:    "waiting for operation to complete",
	})

	if err = s.waitForServerToDisappear(serverName); err != nil {
		logger.WithError(err).Warn("Failed to wait for server to disappear.")
	}

	if err = s.removeObjectPeer(server.ObjectPeerID()); err != nil && !trace.IsNotFound(err) {
		if !force {
			return trace.Wrap(err, "failed to remove the object peer for the node")
		}
		logger.WithError(err).Warn("Failed to remove the object peer for the node, force continue.")
	}

	if err = s.removeClusterStateServers([]string{server.Hostname}); err != nil {
		return trace.Wrap(err)
	}

	_, err = s.compareAndSwapOperationState(context.TODO(), swap{
		key:            opKey,
		expectedStates: []string{ops.OperationStateShrinkInProgress},
		newOpState:     ops.OperationStateCompleted,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	s.reportProgress(opCtx, ops.ProgressEntry{
		State:      ops.ProgressStateCompleted,
		Completion: constants.Completed,
		Message:    fmt.Sprintf("%v removed", serverName),
	})

	return nil
}

func (s *site) pickShrinkMasterRunner(ctx *operationContext, removedServer storage.Server) (*serverRunner, error) {
	masters, err := s.getTeleportServers(schema.ServiceLabelRole, string(schema.ServiceRoleMaster))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Pick any master server except the one that's being removed.
	for _, master := range masters {
		if master.IP != removedServer.AdvertiseIP {
			return &serverRunner{
				&master, &teleportRunner{ctx, s.domainName, s.teleport()},
			}, nil
		}
	}
	return nil, trace.NotFound("%v is being removed and no more master nodes are available to execute the operation",
		removedServer)
}

func (s *site) waitForServerToDisappear(hostname string) error {
	requireServerIsGone := func(domain string, servers []teleservices.Server) error {
		for _, server := range servers {
			labels := server.GetLabels()
			if labels[ops.Hostname] == hostname {
				return trace.AlreadyExists("server %v is not yet removed", hostname)
			}
		}
		return nil
	}

	log.Debug("waiting for server to disappear")
	// wait until the node is removed from the backend
	_, err := s.getTeleportServersWithTimeout(
		nil,
		defaults.TeleportServerQueryTimeout,
		defaults.RetryInterval,
		defaults.RetryLessAttempts,
		requireServerIsGone,
	)
	return trace.Wrap(err)
}

func (s *site) removeFromEtcd(ctx context.Context, opCtx *operationContext, server storage.Server) error {
	peerURL := server.EtcdPeerURL()
	logger := opCtx.WithField("peer", peerURL)
	logger.Info("Remove peer from etcd cluster.")
	b := utils.NewExponentialBackOff(defaults.EtcdRemoveMemberTimeout)
	b.MaxInterval = 10 * time.Second
	return utils.RetryTransient(ctx, b, func() error {
		client, err := clients.DefaultEtcdMembers()
		if err != nil {
			return trace.Wrap(err)
		}
		members, err := client.List(ctx)
		logger.WithError(err).WithField("peers", members).Info("Etcd members.")
		if err != nil {
			return trace.Wrap(err)
		}
		member := utils.EtcdHasMember(members, peerURL)
		if member == nil {
			logger.Info("Peer not found.")
			return nil
		}
		err = client.Remove(ctx, member.ID)
		logger.WithError(err).Info("Removed etcd peer.")
		return trace.Wrap(err)
	})
}

func (s *site) uninstallSystem(ctx *operationContext, runner *serverRunner) error {
	commands := [][]string{
		s.gravityCommand("system", "uninstall",
			"--confirm",
			"--system-log-file", defaults.GravitySystemLogPath),
	}

	for _, command := range commands {
		out, err := runner.Run(command...)
		if err != nil {
			ctx.WithError(err).WithFields(log.Fields{
				"command": command,
				"output":  string(out),
			}).Error("Failed to run.")
		}
	}

	return nil
}

func (s *site) launchAgent(ctx context.Context, opCtx *operationContext, server storage.Server) (*serverRunner, error) {
	teleportServer, err := s.getTeleportServer(ops.Hostname, server.Hostname)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	teleportRunner := &serverRunner{
		server: teleportServer,
		runner: &teleportRunner{opCtx, s.domainName, s.teleport()},
	}

	tokenID, err := s.createShrinkAgentToken(opCtx.operation.ID)
	if err != nil {
		return nil, trace.Wrap(err, "failed to create shrink agent token")
	}

	serverAddr := s.service.cfg.Agents.ServerAddr()
	command := []string{
		"ops", "agent", s.packages().PortalURL(),
		"--advertise-addr", server.AdvertiseIP,
		"--server-addr", serverAddr,
		"--token", tokenID,
		"--vars", fmt.Sprintf("%v:%v", ops.AgentMode, ops.AgentModeShrink),
		"--service-uid", s.uid(),
		"--service-gid", s.gid(),
		"--service-name", defaults.GravityRPCAgentServiceName,
		"--cloud-provider", s.provider,
	}
	out, err := teleportRunner.Run(s.gravityCommand(command...)...)
	if err != nil {
		return nil, trace.Wrap(err, "failed to start shrink agent: %s", out)
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.AgentConnectTimeout)
	defer cancel()
	agentReport, err := s.waitForAgents(ctx, opCtx)
	if err != nil {
		return nil, trace.Wrap(err, "failed to wait for shrink agent")
	}

	if len(agentReport.Servers) == 0 {
		log.Warn(agentReport.Message)
		return nil, trace.NotFound("failed to wait for shrink agent")
	}

	info := agentReport.Servers[0]
	return &serverRunner{
		server: agentServer{
			AdvertiseIP: info.AdvertiseAddr,
			Hostname:    info.GetHostname(),
		},
		runner: &agentRunner{opCtx, s.agentService()},
	}, nil
}

func (s *site) createShrinkAgentToken(operationID string) (tokenID string, err error) {
	token, err := users.CryptoRandomToken(defaults.ProvisioningTokenBytes)
	if err != nil {
		return "", trace.Wrap(err)
	}
	_, err = s.users().CreateProvisioningToken(storage.ProvisioningToken{
		Token:       token,
		AccountID:   s.key.AccountID,
		SiteDomain:  s.key.SiteDomain,
		Type:        storage.ProvisioningTokenTypeInstall,
		Expires:     s.clock().UtcNow().Add(defaults.InstallTokenTTL),
		OperationID: operationID,
		UserEmail:   s.agentUserEmail(),
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	return token, nil
}

// deletePackages removes stale packages generated for the specified server
// from the cluster package service after the server had been removed.
func (s *site) deletePackages(server *ProvisionedServer) error {
	serverPackages, err := s.serverPackages(server)
	if err != nil {
		return trace.Wrap(err)
	}
	for _, pkg := range serverPackages {
		err = s.packages().DeletePackage(pkg)
		if err != nil && !trace.IsNotFound(err) {
			return trace.Wrap(err, "failed to delete package").AddField("package", pkg)
		}
	}
	return nil
}

func (s *site) serverPackages(server *ProvisionedServer) ([]loc.Locator, error) {
	var packages []loc.Locator
	err := pack.ForeachPackage(s.packages(), func(env pack.PackageEnvelope) error {
		if env.HasLabel(pack.AdvertiseIPLabel, server.AdvertiseIP) {
			packages = append(packages, env.Locator)
			return nil
		}
		if s.isTeleportMasterConfigPackageFor(server, env.Locator) ||
			s.isTeleportNodeConfigPackageFor(server, env.Locator) ||
			s.isPlanetConfigPackageFor(server, env.Locator) ||
			s.isPlanetSecretsPackageFor(server, env.Locator) {
			packages = append(packages, env.Locator)
		}
		return nil
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return packages, nil
}

// unlabelNode deletes server profile labels from k8s node
func (s *site) unlabelNode(server storage.Server, runner *serverRunner) error {
	profile, err := s.app.Manifest.NodeProfiles.ByName(server.Role)
	if err != nil {
		return trace.Wrap(err)
	}

	var labelFlags []string
	for label := range profile.Labels {
		labelFlags = append(labelFlags, fmt.Sprintf("%s-", label))
	}

	command := s.planetEnterCommand(defaults.KubectlBin, "label", "nodes",
		fmt.Sprintf("-l=%v=%v", v1.LabelHostname, server.KubeNodeID()))
	command = append(command, labelFlags...)

	err = utils.Retry(defaults.RetryInterval, defaults.RetryAttempts, func() error {
		_, err := runner.Run(command...)
		return trace.Wrap(err)
	})

	return trace.Wrap(err)
}

func (s *site) setElectionStatus(server storage.Server, enable bool, runner *serverRunner) error {
	key := fmt.Sprintf("/planet/cluster/%s/election/%s", s.domainName, server.AdvertiseIP)

	command := s.planetEnterCommand(defaults.EtcdCtlBin,
		"set", key, fmt.Sprintf("%v", enable))

	out, err := runner.Run(command...)
	if err != nil {
		return trace.Wrap(err, "setting leader election on %q to %v: %s", server.AdvertiseIP, enable, out).AddFields(
			map[string]interface{}{
				"cluster":      s.domainName,
				"advertise-ip": server.AdvertiseIP,
				"hostname":     server.Hostname,
			})
	}

	return nil
}

func (s *site) drain(server storage.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.DrainTimeout)
	defer cancel()

	err := utils.RetryWithInterval(ctx, backoff.NewConstantBackOff(time.Second), func() error {
		client, _, err := httplib.GetClusterKubeClient(storage.DefaultDNSConfig.Addr())
		if err != nil {
			return trace.Wrap(err, "failed to create Kubernetes client")
		}

		err = kubernetes.Drain(ctx, client, server.KubeNodeID())
		if err != nil {
			return trace.Wrap(err, "failed to drain node").AddFields(
				map[string]interface{}{
					"cluster":      s.domainName,
					"advertise-ip": server.AdvertiseIP,
					"hostname":     server.Hostname,
					"kube-node-id": server.KubeNodeID(),
				})
		}

		return nil
	})

	return trace.Wrap(err)
}

func (s *site) removeObjectPeer(peerID string) error {
	return trace.Wrap(s.backend().DeletePeer(peerID))
}

func (s *site) removeNodeFromCluster(server storage.Server, runner *serverRunner) (err error) {
	commands := [][]string{
		s.planetEnterCommand(
			defaults.KubectlBin, "delete", "nodes", "--ignore-not-found=true",
			fmt.Sprintf("-l=%v=%v", v1.LabelHostname, server.KubeNodeID())),
	}

	err = utils.Retry(defaults.RetryInterval, defaults.RetryAttempts, func() error {
		for _, command := range commands {
			out, err := runner.Run(command...)
			if err != nil {
				return trace.Wrap(err, "command %q failed: %s", command, out)
			}
		}
		return nil
	})

	return trace.Wrap(err)
}

func (s *site) isTeleportMasterConfigPackageFor(server *ProvisionedServer, loc loc.Locator) bool {
	configPackage := s.teleportMasterConfigPackage(server)
	return configPackage.Name == loc.Name && configPackage.Repository == loc.Repository
}

func (s *site) isTeleportNodeConfigPackageFor(server *ProvisionedServer, loc loc.Locator) bool {
	configPackage := s.teleportNodeConfigPackage(server)
	return configPackage.Name == loc.Name && configPackage.Repository == loc.Repository
}

func (s *site) isPlanetConfigPackageFor(server *ProvisionedServer, loc loc.Locator) bool {
	// Version omitted on purpose since only repository/name are used for comparison
	configPackage := s.planetConfigPackage(server, "")
	return configPackage.Name == loc.Name && configPackage.Repository == loc.Repository
}

func (s *site) isPlanetSecretsPackageFor(server *ProvisionedServer, loc loc.Locator) bool {
	// Version omitted on purpose since only repository/name are used for comparison
	configPackage := s.planetSecretsPackage(server, "")
	return configPackage.Name == loc.Name && configPackage.Repository == loc.Repository
}
