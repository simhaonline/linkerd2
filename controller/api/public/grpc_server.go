package public

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/golang/protobuf/ptypes/duration"
	healthcheckPb "github.com/linkerd/linkerd2/controller/gen/common/healthcheck"
	tapPb "github.com/linkerd/linkerd2/controller/gen/controller/tap"
	pb "github.com/linkerd/linkerd2/controller/gen/public"
	"github.com/linkerd/linkerd2/controller/k8s"
	"github.com/linkerd/linkerd2/pkg/version"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8sV1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

type (
	grpcServer struct {
		prometheusAPI       promv1.API
		tapClient           tapPb.TapClient
		k8sAPI              *k8s.API
		controllerNamespace string
		ignoredNamespaces   []string
	}
)

type podReport struct {
	lastReport              time.Time
	processStartTimeSeconds time.Time
}

const (
	podQuery                   = "max(process_start_time_seconds{%s}) by (pod, namespace)"
	K8sClientSubsystemName     = "kubernetes"
	K8sClientCheckDescription  = "control plane can talk to Kubernetes"
	PromClientSubsystemName    = "prometheus"
	PromClientCheckDescription = "control plane can talk to Prometheus"
)

func newGrpcServer(
	promAPI promv1.API,
	tapClient tapPb.TapClient,
	k8sAPI *k8s.API,
	controllerNamespace string,
	ignoredNamespaces []string,
) *grpcServer {

	k8sAPI.Pod().Informer().AddIndexers(cache.Indexers{k8s.PodIPIndex: k8s.IndexPodByIP})
	return &grpcServer{
		prometheusAPI:       promAPI,
		tapClient:           tapClient,
		k8sAPI:              k8sAPI,
		controllerNamespace: controllerNamespace,
		ignoredNamespaces:   ignoredNamespaces,
	}
}

func (*grpcServer) Version(ctx context.Context, req *pb.Empty) (*pb.VersionInfo, error) {
	return &pb.VersionInfo{GoVersion: runtime.Version(), ReleaseVersion: version.Version, BuildDate: "1970-01-01T00:00:00Z"}, nil
}

func (s *grpcServer) ListPods(ctx context.Context, req *pb.ListPodsRequest) (*pb.ListPodsResponse, error) {
	log.Debugf("ListPods request: %+v", req)

	// Reports is a map from instance name to the absolute time of the most recent
	// report from that instance and its process start time
	reports := make(map[string]podReport)

	nsQuery := ""
	if req.GetNamespace() != "" {
		nsQuery = fmt.Sprintf("namespace=\"%s\"", req.GetNamespace())
	}
	processStartTimeQuery := fmt.Sprintf(podQuery, nsQuery)

	// Query Prometheus for all pods present
	vec, err := s.queryProm(ctx, processStartTimeQuery)
	if err != nil {
		return nil, err
	}
	for _, sample := range vec {
		pod := string(sample.Metric["pod"])
		timestamp := sample.Timestamp

		reports[pod] = podReport{
			lastReport:              time.Unix(0, int64(timestamp)*int64(time.Millisecond)),
			processStartTimeSeconds: time.Unix(0, int64(sample.Value)*int64(time.Second)),
		}
	}

	var pods []*k8sV1.Pod
	namespace := req.GetNamespace()
	if namespace != "" {
		pods, err = s.k8sAPI.Pod().Lister().Pods(namespace).List(labels.Everything())
	} else {
		pods, err = s.k8sAPI.Pod().Lister().List(labels.Everything())
	}

	if err != nil {
		return nil, err
	}
	podList := make([]*pb.Pod, 0)

	for _, pod := range pods {
		if s.shouldIgnore(pod) {
			continue
		}
		item := s.k8sAPI.ToPodProto(pod)

		updated, added := reports[pod.Name]
		item.Added = added

		if added {
			since := time.Since(updated.lastReport)
			item.SinceLastReport = &duration.Duration{
				Seconds: int64(since / time.Second),
				Nanos:   int32(since % time.Second),
			}
			sinceStarting := time.Since(updated.processStartTimeSeconds)
			item.Uptime = &duration.Duration{
				Seconds: int64(sinceStarting / time.Second),
				Nanos:   int32(sinceStarting % time.Second),
			}
		}

		podList = append(podList, item)
	}

	rsp := pb.ListPodsResponse{Pods: podList}

	log.Debugf("ListPods response: %+v", rsp)

	return &rsp, nil
}

func (s *grpcServer) SelfCheck(ctx context.Context, in *healthcheckPb.SelfCheckRequest) (*healthcheckPb.SelfCheckResponse, error) {
	k8sClientCheck := &healthcheckPb.CheckResult{
		SubsystemName:    K8sClientSubsystemName,
		CheckDescription: K8sClientCheckDescription,
		Status:           healthcheckPb.CheckStatus_OK,
	}
	_, err := s.k8sAPI.Pod().Lister().List(labels.Everything())
	if err != nil {
		k8sClientCheck.Status = healthcheckPb.CheckStatus_ERROR
		k8sClientCheck.FriendlyMessageToUser = fmt.Sprintf("Error calling the Kubernetes API: %s", err)
	}

	promClientCheck := &healthcheckPb.CheckResult{
		SubsystemName:    PromClientSubsystemName,
		CheckDescription: PromClientCheckDescription,
		Status:           healthcheckPb.CheckStatus_OK,
	}
	_, err = s.queryProm(ctx, fmt.Sprintf(podQuery, ""))
	if err != nil {
		promClientCheck.Status = healthcheckPb.CheckStatus_ERROR
		promClientCheck.FriendlyMessageToUser = fmt.Sprintf("Error calling Prometheus from the control plane: %s", err)
	}

	response := &healthcheckPb.SelfCheckResponse{
		Results: []*healthcheckPb.CheckResult{
			k8sClientCheck,
			promClientCheck,
		},
	}
	return response, nil
}

func (s *grpcServer) Tap(req *pb.TapRequest, stream pb.Api_TapServer) error {
	return status.Error(codes.Unimplemented, "Tap is deprecated, use TapByResource")
}

// Pass through to tap service
func (s *grpcServer) TapByResource(req *pb.TapByResourceRequest, stream pb.Api_TapByResourceServer) error {
	tapStream := stream.(tapServer)
	tapClient, err := s.tapClient.TapByResource(tapStream.Context(), req)
	if err != nil {
		log.Errorf("Unexpected error tapping [%v]: %v", req, err)
		return err
	}
	for {
		select {
		case <-tapStream.Context().Done():
			return nil
		default:
			event, err := tapClient.Recv()
			if err != nil {
				return err
			}
			tapStream.Send(event)
		}
	}
}

func (s *grpcServer) shouldIgnore(pod *k8sV1.Pod) bool {
	for _, namespace := range s.ignoredNamespaces {
		if pod.Namespace == namespace {
			return true
		}
	}
	return false
}
