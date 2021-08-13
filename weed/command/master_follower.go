package command

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb"
	"github.com/chrislusf/seaweedfs/weed/pb/master_pb"
	"github.com/chrislusf/seaweedfs/weed/security"
	"github.com/chrislusf/seaweedfs/weed/server"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/gorilla/mux"
	"google.golang.org/grpc/reflection"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	mf MasterOptions
)

func init() {
	cmdMasterFollower.Run = runMasterFollower // break init cycle
	mf.port = cmdMasterFollower.Flag.Int("port", 9334, "http listen port")
	mf.ipBind = cmdMasterFollower.Flag.String("ip.bind", "", "ip address to bind to")
	mf.peers = cmdMasterFollower.Flag.String("masters", "localhost:9333", "all master nodes in comma separated ip:port list, example: 127.0.0.1:9093,127.0.0.1:9094,127.0.0.1:9095")

	mf.ip = aws.String(util.DetectedHostAddress())
	mf.metaFolder = aws.String("")
	mf.volumeSizeLimitMB = nil
	mf.volumePreallocate = nil
	mf.defaultReplication = nil
	mf.garbageThreshold = aws.Float64(0.1)
	mf.whiteList = nil
	mf.disableHttp = aws.Bool(false)
	mf.metricsAddress = aws.String("")
	mf.metricsIntervalSec = aws.Int(0)
	mf.raftResumeState = aws.Bool(false)
}

var cmdMasterFollower = &Command{
	UsageLine: "master.follower -port=9333 -masters=<master1Host>:<master1Port>",
	Short:     "start a master follower",
	Long: `start a master follower to provide volume=>location mapping service

	The master follower does not participate in master election. 
	It just follow the existing masters, and listen for any volume location changes.

	In most cases, the master follower is not needed. In big data centers with thousands of volume
	servers. In theory, the master may have trouble to keep up with the write requests and read requests.

	The master follower can relieve the master from from read requests, which only needs to 
	lookup a fileId or volumeId.

	The master follower currently can handle fileId lookup requests:
		/dir/lookup?volumeId=4
		/dir/lookup?fileId=4,49c50924569199
	And gRPC API
		rpc LookupVolume (LookupVolumeRequest) returns (LookupVolumeResponse) {}

	This master follower is stateless and can run from any place.

  `,
}

func runMasterFollower(cmd *Command, args []string) bool {

	util.LoadConfiguration("security", false)
	util.LoadConfiguration("master", false)

	startMasterFollower(mf)

	return true
}

func startMasterFollower(masterOptions MasterOptions) {

	// collect settings from main masters
	masters := strings.Split(*mf.peers, ",")
	masterGrpcAddresses, err := pb.ParseServersToGrpcAddresses(masters)
	if err != nil {
		glog.V(0).Infof("ParseFilerGrpcAddress: %v", err)
		return
	}

	grpcDialOption := security.LoadClientTLS(util.GetViper(), "grpc.master")
	for i := 0; i < 10; i++ {
		err = pb.WithOneOfGrpcMasterClients(masterGrpcAddresses, grpcDialOption, func(client master_pb.SeaweedClient) error {
			resp, err := client.GetMasterConfiguration(context.Background(), &master_pb.GetMasterConfigurationRequest{})
			if err != nil {
				return fmt.Errorf("get master grpc address %v configuration: %v", masterGrpcAddresses, err)
			}
			masterOptions.defaultReplication = &resp.DefaultReplication
			masterOptions.volumeSizeLimitMB = aws.Uint(uint(resp.VolumeSizeLimitMB))
			masterOptions.volumePreallocate = &resp.VolumePreallocate
			return nil
		})
		if err != nil {
			glog.V(0).Infof("failed to talk to filer %v: %v", masterGrpcAddresses, err)
			glog.V(0).Infof("wait for %d seconds ...", i+1)
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}
	if err != nil {
		glog.Errorf("failed to talk to filer %v: %v", masterGrpcAddresses, err)
		return
	}


	option := masterOptions.toMasterOption(nil)
	option.IsFollower = true


	r := mux.NewRouter()
	ms := weed_server.NewMasterServer(r, option, masters)
	listeningAddress := *masterOptions.ipBind + ":" + strconv.Itoa(*masterOptions.port)
	glog.V(0).Infof("Start Seaweed Master %s at %s", util.Version(), listeningAddress)
	masterListener, e := util.NewListener(listeningAddress, 0)
	if e != nil {
		glog.Fatalf("Master startup error: %v", e)
	}

	// starting grpc server
	grpcPort := *masterOptions.port + 10000
	grpcL, err := util.NewListener(*masterOptions.ipBind+":"+strconv.Itoa(grpcPort), 0)
	if err != nil {
		glog.Fatalf("master failed to listen on grpc port %d: %v", grpcPort, err)
	}
	grpcS := pb.NewGrpcServer(security.LoadServerTLS(util.GetViper(), "grpc.master"))
	master_pb.RegisterSeaweedServer(grpcS, ms)
	reflection.Register(grpcS)
	glog.V(0).Infof("Start Seaweed Master %s grpc server at %s:%d", util.Version(), *masterOptions.ip, grpcPort)
	go grpcS.Serve(grpcL)

	go ms.MasterClient.KeepConnectedToMaster()

	// start http server
	httpS := &http.Server{Handler: r}
	go httpS.Serve(masterListener)

	select {}
}