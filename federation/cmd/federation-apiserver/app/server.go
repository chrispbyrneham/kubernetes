/*
Copyright 2014 The Kubernetes Authors.

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

// Package app does all of the work necessary to create a Kubernetes
// APIServer by binding together the API, master and APIServer infrastructure.
// It can be configured and called directly or via the hyperkube cache.
package app

import (
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/pborman/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"k8s.io/kubernetes/federation/cmd/federation-apiserver/app/options"
	"k8s.io/kubernetes/pkg/admission"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apis/rbac"
	"k8s.io/kubernetes/pkg/apiserver/authenticator"
	authorizerunion "k8s.io/kubernetes/pkg/auth/authorizer/union"
	"k8s.io/kubernetes/pkg/auth/user"
	"k8s.io/kubernetes/pkg/controller/informers"
	"k8s.io/kubernetes/pkg/generated/openapi"
	"k8s.io/kubernetes/pkg/genericapiserver"
	"k8s.io/kubernetes/pkg/genericapiserver/authorizer"
	genericoptions "k8s.io/kubernetes/pkg/genericapiserver/options"
	genericvalidation "k8s.io/kubernetes/pkg/genericapiserver/validation"
	"k8s.io/kubernetes/pkg/registry/cachesize"
	"k8s.io/kubernetes/pkg/registry/generic"
	"k8s.io/kubernetes/pkg/registry/generic/registry"
	"k8s.io/kubernetes/pkg/registry/rbac/clusterrole"
	clusterroleetcd "k8s.io/kubernetes/pkg/registry/rbac/clusterrole/etcd"
	"k8s.io/kubernetes/pkg/registry/rbac/clusterrolebinding"
	clusterrolebindingetcd "k8s.io/kubernetes/pkg/registry/rbac/clusterrolebinding/etcd"
	"k8s.io/kubernetes/pkg/registry/rbac/role"
	roleetcd "k8s.io/kubernetes/pkg/registry/rbac/role/etcd"
	"k8s.io/kubernetes/pkg/registry/rbac/rolebinding"
	rolebindingetcd "k8s.io/kubernetes/pkg/registry/rbac/rolebinding/etcd"
	"k8s.io/kubernetes/pkg/routes"
	"k8s.io/kubernetes/pkg/util/wait"
	authenticatorunion "k8s.io/kubernetes/plugin/pkg/auth/authenticator/request/union"
)

// NewAPIServerCommand creates a *cobra.Command object with default parameters
func NewAPIServerCommand() *cobra.Command {
	s := options.NewServerRunOptions()
	s.AddFlags(pflag.CommandLine)
	cmd := &cobra.Command{
		Use: "federation-apiserver",
		Long: `The Kubernetes federation API server validates and configures data
for the api objects which include pods, services, replicationcontrollers, and
others. The API Server services REST operations and provides the frontend to the
cluster's shared state through which all other components interact.`,
		Run: func(cmd *cobra.Command, args []string) {
		},
	}
	return cmd
}

// Run runs the specified APIServer.  This should never exit.
func Run(s *options.ServerRunOptions) error {
	genericvalidation.VerifyEtcdServersList(s.ServerRunOptions)
	genericapiserver.DefaultAndValidateRunOptions(s.ServerRunOptions)

	// TODO: register cluster federation resources here.
	resourceConfig := genericapiserver.NewResourceConfig()

	storageGroupsToEncodingVersion, err := s.StorageGroupsToEncodingVersion()
	if err != nil {
		glog.Fatalf("error generating storage version map: %s", err)
	}
	storageFactory, err := genericapiserver.BuildDefaultStorageFactory(
		s.StorageConfig, s.DefaultStorageMediaType, api.Codecs,
		genericapiserver.NewDefaultResourceEncodingConfig(), storageGroupsToEncodingVersion,
		[]unversioned.GroupVersionResource{}, resourceConfig, s.RuntimeConfig)
	if err != nil {
		glog.Fatalf("error in initializing storage factory: %s", err)
	}

	for _, override := range s.EtcdServersOverrides {
		tokens := strings.Split(override, "#")
		if len(tokens) != 2 {
			glog.Errorf("invalid value of etcd server overrides: %s", override)
			continue
		}

		apiresource := strings.Split(tokens[0], "/")
		if len(apiresource) != 2 {
			glog.Errorf("invalid resource definition: %s", tokens[0])
			continue
		}
		group := apiresource[0]
		resource := apiresource[1]
		groupResource := unversioned.GroupResource{Group: group, Resource: resource}

		servers := strings.Split(tokens[1], ";")
		storageFactory.SetEtcdLocation(groupResource, servers)
	}

	apiAuthenticator, err := authenticator.New(authenticator.AuthenticatorConfig{
		BasicAuthFile:     s.BasicAuthFile,
		ClientCAFile:      s.ClientCAFile,
		TokenAuthFile:     s.TokenAuthFile,
		OIDCIssuerURL:     s.OIDCIssuerURL,
		OIDCClientID:      s.OIDCClientID,
		OIDCCAFile:        s.OIDCCAFile,
		OIDCUsernameClaim: s.OIDCUsernameClaim,
		OIDCGroupsClaim:   s.OIDCGroupsClaim,
		KeystoneURL:       s.KeystoneURL,
	})
	if err != nil {
		glog.Fatalf("Invalid Authentication Config: %v", err)
	}

	authorizationModeNames := strings.Split(s.AuthorizationMode, ",")

	modeEnabled := func(mode string) bool {
		for _, m := range authorizationModeNames {
			if m == mode {
				return true
			}
		}
		return false
	}

	authorizationConfig := authorizer.AuthorizationConfig{
		PolicyFile:                  s.AuthorizationPolicyFile,
		WebhookConfigFile:           s.AuthorizationWebhookConfigFile,
		WebhookCacheAuthorizedTTL:   s.AuthorizationWebhookCacheAuthorizedTTL,
		WebhookCacheUnauthorizedTTL: s.AuthorizationWebhookCacheUnauthorizedTTL,
		RBACSuperUser:               s.AuthorizationRBACSuperUser,
	}
	if modeEnabled(genericoptions.ModeRBAC) {
		mustGetRESTOptions := func(resource string) generic.RESTOptions {
			config, err := storageFactory.NewConfig(rbac.Resource(resource))
			if err != nil {
				glog.Fatalf("Unable to get %s storage: %v", resource, err)
			}
			return generic.RESTOptions{StorageConfig: config, Decorator: generic.UndecoratedStorage, ResourcePrefix: storageFactory.ResourcePrefix(rbac.Resource(resource))}
		}

		// For initial bootstrapping go directly to etcd to avoid privillege escalation check.
		authorizationConfig.RBACRoleRegistry = role.NewRegistry(roleetcd.NewREST(mustGetRESTOptions("roles")))
		authorizationConfig.RBACRoleBindingRegistry = rolebinding.NewRegistry(rolebindingetcd.NewREST(mustGetRESTOptions("rolebindings")))
		authorizationConfig.RBACClusterRoleRegistry = clusterrole.NewRegistry(clusterroleetcd.NewREST(mustGetRESTOptions("clusterroles")))
		authorizationConfig.RBACClusterRoleBindingRegistry = clusterrolebinding.NewRegistry(clusterrolebindingetcd.NewREST(mustGetRESTOptions("clusterrolebindings")))
	}

	apiAuthorizer, err := authorizer.NewAuthorizerFromAuthorizationConfig(authorizationModeNames, authorizationConfig)
	if err != nil {
		glog.Fatalf("Invalid Authorization Config: %v", err)
	}

	admissionControlPluginNames := strings.Split(s.AdmissionControl, ",")
	privilegedLoopbackToken := uuid.NewRandom().String()

	client, err := s.NewSelfClient(privilegedLoopbackToken)
	if err != nil {
		glog.Errorf("Failed to create clientset: %v", err)
	}

	// TODO(dims): We probably need to add an option "EnableLoopbackToken"
	if apiAuthenticator != nil {
		var uid = uuid.NewRandom().String()
		tokens := make(map[string]*user.DefaultInfo)
		tokens[privilegedLoopbackToken] = &user.DefaultInfo{
			Name:   "system:apiserver",
			UID:    uid,
			Groups: []string{"system:masters"},
		}

		tokenAuthenticator := authenticator.NewAuthenticatorFromTokens(tokens)
		apiAuthenticator = authenticatorunion.New(tokenAuthenticator, apiAuthenticator)

		tokenAuthorizer := authorizer.NewPrivilegedGroups("system:masters")
		apiAuthorizer = authorizerunion.New(tokenAuthorizer, apiAuthorizer)
	}

	sharedInformers := informers.NewSharedInformerFactory(client, 10*time.Minute)
	pluginInitializer := admission.NewPluginInitializer(sharedInformers)

	admissionController, err := admission.NewFromPlugins(client, admissionControlPluginNames, s.AdmissionControlConfigFile, pluginInitializer)
	if err != nil {
		glog.Fatalf("Failed to initialize plugins: %v", err)
	}
	genericConfig := genericapiserver.NewConfig(s.ServerRunOptions)
	// TODO: Move the following to generic api server as well.
	genericConfig.Authenticator = apiAuthenticator
	genericConfig.SupportsBasicAuth = len(s.BasicAuthFile) > 0
	genericConfig.Authorizer = apiAuthorizer
	genericConfig.AuthorizerRBACSuperUser = s.AuthorizationRBACSuperUser
	genericConfig.AdmissionControl = admissionController
	genericConfig.APIResourceConfigSource = storageFactory.APIResourceConfigSource
	genericConfig.MasterServiceNamespace = s.MasterServiceNamespace
	genericConfig.Serializer = api.Codecs
	genericConfig.OpenAPIDefinitions = openapi.OpenAPIDefinitions
	genericConfig.EnableOpenAPISupport = true

	// TODO: Move this to generic api server (Need to move the command line flag).
	if s.EnableWatchCache {
		cachesize.InitializeWatchCacheSizes(s.TargetRAMMB)
		cachesize.SetWatchCacheSizes(s.WatchCacheSizes)
	}

	m, err := genericConfig.Complete().New()
	if err != nil {
		return err
	}

	routes.UIRedirect{}.Install(m.Mux, m.HandlerContainer)
	routes.Logs{}.Install(m.Mux, m.HandlerContainer)

	restOptionsFactory := restOptionsFactory{
		storageFactory:          storageFactory,
		deleteCollectionWorkers: s.DeleteCollectionWorkers,
	}
	if s.EnableWatchCache {
		restOptionsFactory.storageDecorator = registry.StorageWithCacher
	} else {
		restOptionsFactory.storageDecorator = generic.UndecoratedStorage
	}

	installFederationAPIs(m, restOptionsFactory)
	installCoreAPIs(s, m, restOptionsFactory)
	installExtensionsAPIs(m, restOptionsFactory)

	sharedInformers.Start(wait.NeverStop)
	m.Run(s.ServerRunOptions)
	return nil
}

type restOptionsFactory struct {
	storageFactory          genericapiserver.StorageFactory
	storageDecorator        generic.StorageDecorator
	deleteCollectionWorkers int
}

func (f restOptionsFactory) NewFor(resource unversioned.GroupResource) generic.RESTOptions {
	config, err := f.storageFactory.NewConfig(resource)
	if err != nil {
		glog.Fatalf("Unable to find storage config for %v, due to %v", resource, err.Error())
	}
	return generic.RESTOptions{
		StorageConfig:           config,
		Decorator:               f.storageDecorator,
		DeleteCollectionWorkers: f.deleteCollectionWorkers,
		ResourcePrefix:          f.storageFactory.ResourcePrefix(resource),
	}
}
