package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"

	cf_http "code.cloudfoundry.org/cfhttp"
	cf_lager "code.cloudfoundry.org/cflager"
	cf_debug_server "code.cloudfoundry.org/debugserver"

	"code.cloudfoundry.org/goshims/execshim"
	"code.cloudfoundry.org/goshims/filepathshim"
	"code.cloudfoundry.org/goshims/ioutilshim"
	"code.cloudfoundry.org/goshims/osshim"
	"code.cloudfoundry.org/goshims/usershim"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/nfsdriver"
	"code.cloudfoundry.org/voldriver"
	"code.cloudfoundry.org/voldriver/driverhttp"
	"github.com/lds-cf/knfsdriver/authorizer"
	"github.com/lds-cf/knfsdriver/kerberizer"
	"github.com/lds-cf/knfsdriver/mounter"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"
)

var atAddress = flag.String(
	"listenAddr",
	"0.0.0.0:9750",
	"host:port to serve volume management functions",
)

var nfsVolToolsAddress = flag.String(
	"nfsVolToolsAddr",
	"",
	"host:port to serve nfs volume tools functions (for drivers colocated with the nfs broker)",
)

var driversPath = flag.String(
	"driversPath",
	"",
	"Path to directory where drivers are installed",
)

var transport = flag.String(
	"transport",
	"tcp",
	"Transport protocol to transmit HTTP over",
)

var mountDir = flag.String(
	"mountDir",
	"/tmp/volumes",
	"Path to directory where fake volumes are created",
)

var requireSSL = flag.Bool(
	"requireSSL",
	false,
	"whether the fake driver should require ssl-secured communication",
)

var caFile = flag.String(
	"caFile",
	"",
	"the certificate authority public key file to use with ssl authentication",
)

var certFile = flag.String(
	"certFile",
	"",
	"the public key file to use with ssl authentication",
)

var keyFile = flag.String(
	"keyFile",
	"",
	"the private key file to use with ssl authentication",
)
var clientCertFile = flag.String(
	"clientCertFile",
	"",
	"the public key file to use with client ssl authentication",
)

var clientKeyFile = flag.String(
	"clientKeyFile",
	"",
	"the private key file to use with client ssl authentication",
)

var principal = flag.String(
	"kerberosPrincipal",
	"",
	"the Kerberos principal for the Cloud Foundry service account",
)

var keytab = flag.String(
	"keytab",
	"",
	"the keytab file containing encrypted secret for the kerberosPrincipal",
)

var insecureSkipVerify = flag.Bool(
	"insecureSkipVerify",
	false,
	"whether SSL communication should skip verification of server IP addresses in the certificate",
)

func main() {
	parseCommandLine()

	var localDriverServer ifrit.Runner

	logger, logTap := newLogger()
	logger.Info("start")
	defer logger.Info("end")

	kerberizer := kerberizer.NewKerberizer(&execshim.ExecShim{})
	kerberizer.Login(logger, *principal, *keytab)

	authorizer := authorizer.NewAuthorizer(kerberizer, &execshim.ExecShim{}, &usershim.UserShim{})

	client := nfsdriver.NewNfsDriver(
		logger,
		&osshim.OsShim{},
		&filepathshim.FilepathShim{},
		&ioutilshim.IoutilShim{},
		*mountDir,
		mounter.NewNfsMounter(authorizer, &execshim.ExecShim{}, &ioutilshim.IoutilShim{}),
	)

	if *transport == "tcp" {
		localDriverServer = createNfsDriverServer(logger, client, *atAddress, *driversPath, false, *nfsVolToolsAddress)
	} else if *transport == "tcp-json" {
		localDriverServer = createNfsDriverServer(logger, client, *atAddress, *driversPath, true, *nfsVolToolsAddress)
	} else {
		localDriverServer = createNfsDriverUnixServer(logger, client, *atAddress)
	}

	servers := grouper.Members{
		{"localdriver-server", localDriverServer},
	}

	if dbgAddr := cf_debug_server.DebugAddress(flag.CommandLine); dbgAddr != "" {
		servers = append(grouper.Members{
			{"debug-server", cf_debug_server.Runner(dbgAddr, logTap)},
		}, servers...)
	}

	process := ifrit.Invoke(processRunnerFor(servers))
	logger.Info("started")

	untilTerminated(logger, process)
}

func exitOnFailure(logger lager.Logger, err error) {
	if err != nil {
		logger.Error("fatal-err..aborting", err)
		panic(err.Error())
	}
}

func untilTerminated(logger lager.Logger, process ifrit.Process) {
	err := <-process.Wait()
	exitOnFailure(logger, err)
}

func processRunnerFor(servers grouper.Members) ifrit.Runner {
	return sigmon.New(grouper.NewOrdered(os.Interrupt, servers))
}

func createNfsDriverServer(logger lager.Logger, client *nfsdriver.NfsDriver, atAddress, driversPath string, jsonSpec bool, nfsToolsAddress string) ifrit.Runner {
	advertisedUrl := "http://" + atAddress
	logger.Info("writing-spec-file", lager.Data{"location": driversPath, "name": "knfsdriver", "address": advertisedUrl})
	if jsonSpec {
		driverJsonSpec := voldriver.DriverSpec{Name: "knfsdriver", Address: advertisedUrl}

		if *requireSSL {
			absCaFile, err := filepath.Abs(*caFile)
			exitOnFailure(logger, err)
			absClientCertFile, err := filepath.Abs(*clientCertFile)
			exitOnFailure(logger, err)
			absClientKeyFile, err := filepath.Abs(*clientKeyFile)
			exitOnFailure(logger, err)
			driverJsonSpec.TLSConfig = &voldriver.TLSConfig{InsecureSkipVerify: *insecureSkipVerify, CAFile: absCaFile, CertFile: absClientCertFile, KeyFile: absClientKeyFile}
			driverJsonSpec.Address = "https://" + atAddress
		}

		jsonBytes, err := json.Marshal(driverJsonSpec)

		exitOnFailure(logger, err)
		err = voldriver.WriteDriverSpec(logger, driversPath, "knfsdriver", "json", jsonBytes)
		exitOnFailure(logger, err)
	} else {
		err := voldriver.WriteDriverSpec(logger, driversPath, "knfsdriver", "spec", []byte(advertisedUrl))
		exitOnFailure(logger, err)
	}

	handler, err := driverhttp.NewHandler(logger, client)
	exitOnFailure(logger, err)

	var server ifrit.Runner
	if *requireSSL {
		tlsConfig, err := cf_http.NewTLSConfig(*certFile, *keyFile, *caFile)
		if err != nil {
			logger.Fatal("tls-configuration-failed", err)
		}
		server = http_server.NewTLSServer(atAddress, handler, tlsConfig)
	} else {
		server = http_server.New(atAddress, handler)
	}

	return server
}
func createNfsDriverUnixServer(logger lager.Logger, client *nfsdriver.NfsDriver, atAddress string) ifrit.Runner {
	handler, err := driverhttp.NewHandler(logger, client)
	exitOnFailure(logger, err)
	return http_server.NewUnixServer(atAddress, handler)
}

func newLogger() (lager.Logger, *lager.ReconfigurableSink) {
	logger, reconfigurableSink := cf_lager.New("nfs-driver-server")
	return logger, reconfigurableSink
}

func parseCommandLine() {
	cf_lager.AddFlags(flag.CommandLine)
	cf_debug_server.AddFlags(flag.CommandLine)
	flag.Parse()
}
