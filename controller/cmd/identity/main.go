package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/golang/protobuf/ptypes"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	idctl "github.com/linkerd/linkerd2/controller/identity"
	"github.com/linkerd/linkerd2/controller/k8s"
	"github.com/linkerd/linkerd2/pkg/admin"
	"github.com/linkerd/linkerd2/pkg/config"
	"github.com/linkerd/linkerd2/pkg/flags"
	"github.com/linkerd/linkerd2/pkg/identity"
	consts "github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/tls"
)

// TODO watch trustAnchorsPath for changes
// TODO watch issuerPath for changes
// TODO restrict servicetoken audiences (and lifetimes)
func main() {
	addr := flag.String("addr", ":8083", "address to serve on")
	adminAddr := flag.String("admin-addr", ":9996", "address of HTTP admin server")
	kubeConfigPath := flag.String("kubeconfig", "", "path to kube config")
	issuerPath := flag.String("issuer",
		"/var/run/linkerd/identity/issuer",
		"path to directoring containing issuer credentials")
	flags.ConfigureAndParse()

	cfg, err := config.Global(consts.MountPathGlobalConfig)
	if err != nil {
		log.Fatalf("Failed to load config: %s", err.Error())
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	controllerNS := cfg.GetLinkerdNamespace()
	idctx := cfg.GetIdentityContext()
	if idctx == nil {
		log.Infof("Identity disabled in control plane configuration.")
		os.Exit(0)
	}

	trustDomain := idctx.GetTrustDomain()
	dom, err := idctl.NewTrustDomain(controllerNS, trustDomain)
	if err != nil {
		log.Fatalf("Invalid trust domain: %s", err.Error())
	}

	trustAnchors, err := tls.DecodePEMCertPool(idctx.GetTrustAnchorsPem())
	if err != nil {
		log.Fatalf("Failed to read trust anchors: %s", err)
	}

	creds, err := tls.ReadPEMCreds(filepath.Join(*issuerPath, "key.pem"), filepath.Join(*issuerPath, "crt.pem"))
	if err != nil {
		log.Fatalf("Failed to read CA from %s: %s", *issuerPath, err)
	}

	expectedName := fmt.Sprintf("identity.%s.%s", controllerNS, trustDomain)
	if err := creds.Crt.Verify(trustAnchors, expectedName); err != nil {
		log.Fatalf("Failed to verify issuer credentials for '%s' with trust anchors: %s", expectedName, err)
	}

	csa := 0 * time.Minute
	if pbd := idctx.GetClockSkewAllowance(); pbd != nil {
		if d, err := ptypes.Duration(pbd); err == nil {
			csa = d
		}
	}

	il := 24 * time.Hour
	if pbd := idctx.GetIssuanceLifetime(); pbd != nil {
		if d, err := ptypes.Duration(pbd); err == nil {
			il = d
		}
	}

	ca := tls.NewCA(*creds, tls.Validity{
		ClockSkewAllowance: csa,
		Lifetime:           il,
	})
	if err != nil {
		log.Fatalf("Failed to read issuer credentials from %s: %s", *issuerPath, err)
	}

	k8s, err := k8s.NewClientSet(*kubeConfigPath)
	if err != nil {
		log.Fatalf("Failed to load kubeconfig: %s: %s", *kubeConfigPath, err)
	}
	v, err := idctl.NewK8sTokenValidator(k8s, dom)
	if err != nil {
		log.Fatalf("Failed to initialize identity service: %s", err)
	}

	svc := identity.NewService(v, ca)

	go admin.StartServer(*adminAddr)
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %s", *addr, err)
	}

	srv := grpc.NewServer()
	identity.Register(srv, svc)
	go func() {
		log.Infof("starting gRPC server on %s", *addr)
		srv.Serve(lis)
	}()
	<-stop
	log.Infof("shutting down gRPC server on %s", *addr)
	srv.GracefulStop()
}
