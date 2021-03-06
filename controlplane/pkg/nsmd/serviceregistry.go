package nsmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/apis/local/networkservice"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/apis/nsmdapi"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/apis/registry"
	remote_networkservice "github.com/networkservicemesh/networkservicemesh/controlplane/pkg/apis/remote/networkservice"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/model"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/serviceregistry"
	"github.com/networkservicemesh/networkservicemesh/controlplane/pkg/vni"
	dataplaneapi "github.com/networkservicemesh/networkservicemesh/dataplane/pkg/apis/dataplane"
	"github.com/networkservicemesh/networkservicemesh/pkg/tools"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

const (
	ServerSock             = "/var/lib/networkservicemesh/nsm.io.sock"
	NsmDevicePluginEnv     = "NSM_DEVICE_PLUGIN"
	folderMask             = 0777
	NsmdApiAddressEnv      = "NSMD_API_ADDRESS"
	NsmdApiAddressDefaults = "0.0.0.0:5001"
)

type apiRegistry struct {
}

func (*apiRegistry) NewPublicListener() (net.Listener, error) {
	nsmdApiAddress := os.Getenv(NsmdApiAddressEnv)
	if strings.TrimSpace(nsmdApiAddress) == "" {
		nsmdApiAddress = NsmdApiAddressDefaults
	}

	return net.Listen("tcp", nsmdApiAddress)
}

func (*apiRegistry) NewNSMServerListener() (net.Listener, error) {
	logrus.Infof("Starting NSM gRPC server listening on socket: %s", ServerSock)
	if err := tools.SocketCleanup(ServerSock); err != nil {
		return nil, err
	}
	return net.Listen("unix", ServerSock)
}

func NewApiRegistry() serviceregistry.ApiRegistry {
	return &apiRegistry{}
}

type nsmdServiceRegistry struct {
	sync.RWMutex
	registryClientConnection *grpc.ClientConn
	stopRedial               bool
	vniAllocator             vni.VniAllocator
	registryAddress          string
}

func (impl *nsmdServiceRegistry) NewWorkspaceProvider() serviceregistry.WorkspaceLocationProvider {
	return NewDefaultWorkspaceProvider()
}

func (impl *nsmdServiceRegistry) RemoteNetworkServiceClient(nsm *registry.NetworkServiceManager) (remote_networkservice.NetworkServiceClient, *grpc.ClientConn, error) {
	err := tools.WaitForPortAvailable(context.Background(), "tcp", nsm.GetUrl(), 1*time.Second)
	if err != nil {
		return nil, nil, err
	}

	logrus.Infof("Remote Network Service %s is available at %s, attempting to connect...", nsm.GetName(), nsm.GetUrl())
	tracer := opentracing.GlobalTracer()
	conn, err := grpc.Dial(nsm.Url, grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(
			otgrpc.OpenTracingClientInterceptor(tracer, otgrpc.LogPayloads())),
		grpc.WithStreamInterceptor(
			otgrpc.OpenTracingStreamClientInterceptor(tracer)))
	if err != nil {
		logrus.Errorf("Failed to dial Network Service Registry %s at %s: %s", nsm.GetName(), nsm.Url, err)
		return nil, nil, err
	}
	client := remote_networkservice.NewNetworkServiceClient(conn)
	return client, conn, nil
}

func (impl *nsmdServiceRegistry) EndpointConnection(endpoint *registry.NSERegistration) (networkservice.NetworkServiceClient, *grpc.ClientConn, error) {
	workspace := WorkSpaceRegistry().WorkspaceByEndpoint(endpoint.GetNetworkserviceEndpoint())
	if workspace == nil {
		err := fmt.Errorf("cannot find workspace for endpoint %v", endpoint)
		logrus.Error(err)
		return nil, nil, err
	}
	nseConn, err := tools.SocketOperationCheck(workspace.NsmClientSocket())
	if err != nil {
		logrus.Errorf("unable to connect to nse %v", endpoint)
		return nil, nil, err
	}
	client := networkservice.NewNetworkServiceClient(nseConn)

	return client, nseConn, nil
}

func (impl *nsmdServiceRegistry) WorkspaceName(endpoint *registry.NSERegistration) string {
	// TODO - this is terribly dirty and needs to be fixed
	workspace := WorkSpaceRegistry().WorkspaceByEndpoint(endpoint.GetNetworkserviceEndpoint())
	if workspace != nil { // In case of tests this could be empty
		return workspace.Name()
	}
	return ""
}

func (impl *nsmdServiceRegistry) DataplaneConnection(dataplane *model.Dataplane) (dataplaneapi.DataplaneClient, *grpc.ClientConn, error) {
	dataplaneConn, err := tools.SocketOperationCheck(dataplane.SocketLocation)
	if err != nil {
		return nil, nil, err
	}
	dpClient := dataplaneapi.NewDataplaneClient(dataplaneConn)
	return dpClient, dataplaneConn, nil
}

func (impl *nsmdServiceRegistry) NSMDApiClient() (nsmdapi.NSMDClient, *grpc.ClientConn, error) {
	logrus.Infof("Connecting to nsmd on socket: %s...", ServerSock)
	if _, err := os.Stat(ServerSock); err != nil {
		return nil, nil, err
	}

	conn, err := tools.SocketOperationCheck(ServerSock)
	if err != nil {
		return nil, nil, err
	}

	logrus.Info("Requesting nsmd for client connection...")
	return nsmdapi.NewNSMDClient(conn), conn, nil
}

func (impl *nsmdServiceRegistry) RegistryClient() (registry.NetworkServiceRegistryClient, error) {
	impl.RWMutex.Lock()
	defer impl.RWMutex.Unlock()

	logrus.Info("Requesting RegistryClient...")

	impl.initRegistryClient()
	if impl.registryClientConnection != nil {
		return registry.NewNetworkServiceRegistryClient(impl.registryClientConnection), nil
	}
	return nil, fmt.Errorf("Connection to Network Registry Server is not available")
}

func (impl *nsmdServiceRegistry) GetPublicAPI() string {
	return GetLocalIPAddress() + ":5001"
}

func (impl *nsmdServiceRegistry) NetworkServiceDiscovery() (registry.NetworkServiceDiscoveryClient, error) {
	impl.RWMutex.Lock()
	defer impl.RWMutex.Unlock()

	logrus.Info("Requesting NetworkServiceDiscoveryClient...")

	impl.initRegistryClient()
	if impl.registryClientConnection != nil {
		return registry.NewNetworkServiceDiscoveryClient(impl.registryClientConnection), nil
	}
	return nil, fmt.Errorf("Connection to Network Registry Server is not available")
}

func (impl *nsmdServiceRegistry) initRegistryClient() {
	var err error
	if impl.registryClientConnection != nil && impl.registryClientConnection.GetState() == connectivity.Ready {
		return // Connection already established.
	}
	// TODO doing registry Address here is ugly
	for impl.stopRedial {
		tools.WaitForPortAvailable(context.Background(), "tcp", impl.registryAddress, 1*time.Second)
		logrus.Println("Registry port now available, attempting to connect...")
		tracer := opentracing.GlobalTracer()
		conn, err := grpc.Dial(impl.registryAddress, grpc.WithInsecure(),
			grpc.WithUnaryInterceptor(
				otgrpc.OpenTracingClientInterceptor(tracer, otgrpc.LogPayloads())),
			grpc.WithStreamInterceptor(
				otgrpc.OpenTracingStreamClientInterceptor(tracer)))
		if err != nil {
			logrus.Errorf("Failed to dial Network Service Registry at %s: %s", impl.registryAddress, err)
			continue
		}
		impl.registryClientConnection = conn
		logrus.Infof("Successfully connected to %s", impl.registryAddress)
		return
	}
	err = fmt.Errorf("stopped before success trying to dial Network Registry Server")
	logrus.Error(err)
}

func (impl *nsmdServiceRegistry) Stop() {
	// I know the stopRedial isn't threadsafe... we don't care, its set once at creation to true
	// so if you set it to false, eventually the redial loop will notice and stop.
	impl.stopRedial = false
	impl.RWMutex.Lock()
	defer impl.RWMutex.Unlock()

	if impl.registryClientConnection != nil {
		impl.registryClientConnection.Close()
	}
}

func NewServiceRegistry() serviceregistry.ServiceRegistry {
	registryAddress := os.Getenv("NSM_REGISTRY_ADDRESS")
	registryAddress = strings.TrimSpace(registryAddress)
	if registryAddress == "" {
		registryAddress = "127.0.0.1:5000"
	}

	return NewServiceRegistryAt(registryAddress)
}

func NewServiceRegistryAt(nsmAddress string) serviceregistry.ServiceRegistry {
	return &nsmdServiceRegistry{
		stopRedial:      true,
		vniAllocator:    vni.NewVniAllocator(),
		registryAddress: nsmAddress,
	}
}

func (impl *nsmdServiceRegistry) WaitForDataplaneAvailable(model model.Model) {
	logrus.Info("Waiting for dataplane available...")
	for ; true; <-time.After(100 * time.Millisecond) {
		if dp, _ := model.SelectDataplane(); dp != nil {
			break
		}
	}
}

func (impl *nsmdServiceRegistry) VniAllocator() vni.VniAllocator {
	return impl.vniAllocator
}

type defaultWorkspaceProvider struct {
	hostBaseDir     string
	nsmBaseDir      string
	clientBaseDir   string
	nsmServerSocket string
	nsmClientSocket string
}

func NewDefaultWorkspaceProvider() serviceregistry.WorkspaceLocationProvider {
	return NewWorkspaceProvider("/var/lib/networkservicemesh/")
}

func NewWorkspaceProvider(rootDir string) serviceregistry.WorkspaceLocationProvider {
	if rootDir[len(rootDir)-1:] != "/" {
		rootDir += "/"
	}
	return &defaultWorkspaceProvider{
		hostBaseDir:     rootDir,
		nsmBaseDir:      rootDir,
		clientBaseDir:   rootDir,
		nsmServerSocket: "nsm.server.io.sock",
		nsmClientSocket: "nsm.client.io.sock",
	}
}

func (w *defaultWorkspaceProvider) HostBaseDir() string {
	return w.hostBaseDir
}

func (w *defaultWorkspaceProvider) NsmBaseDir() string {
	return w.nsmBaseDir
}

func (w *defaultWorkspaceProvider) ClientBaseDir() string {
	return w.clientBaseDir
}

func (w *defaultWorkspaceProvider) NsmServerSocket() string {
	return w.nsmServerSocket
}

func (w *defaultWorkspaceProvider) NsmClientSocket() string {
	return w.nsmClientSocket
}
