/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package chaincode

import (
	"fmt"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/core/chaincode/platforms"
	"github.com/hyperledger/fabric/core/common/ccprovider"
	"github.com/hyperledger/fabric/core/common/sysccprovider"
	"github.com/hyperledger/fabric/core/container/ccintf"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/peer"
	pb "github.com/hyperledger/fabric/protos/peer"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// Runtime is used to manage chaincode runtime instances.
type Runtime interface {
	Start(ccci *ccprovider.ChaincodeContainerInfo, codePackage []byte) error
	Stop(ccci *ccprovider.ChaincodeContainerInfo) error
}

// Launcher is used to launch chaincode runtimes.
type Launcher interface {
	Launch(ccci *ccprovider.ChaincodeContainerInfo) error
}

// Lifecycle provides a way to retrieve chaincode definitions and the packages necessary to run them
type Lifecycle interface {
	// ChaincodeDefinition returns the details for a chaincode by name
	ChaincodeDefinition(chaincodeName string, txSim ledger.QueryExecutor) (ccprovider.ChaincodeDefinition, error)

	// ChaincodeContainerInfo returns the package necessary to launch a chaincode
	ChaincodeContainerInfo(chainID string, chaincodeID string) (*ccprovider.ChaincodeContainerInfo, error)
}

// ChaincodeSupport responsible for providing interfacing with chaincodes from the Peer.
type ChaincodeSupport struct {
	Keepalive        time.Duration
	ExecuteTimeout   time.Duration
	UserRunsCC       bool
	Runtime          Runtime
	ACLProvider      ACLProvider
	HandlerRegistry  *HandlerRegistry
	Launcher         Launcher
	SystemCCProvider sysccprovider.SystemChaincodeProvider
	Lifecycle        Lifecycle
}

// NewChaincodeSupport creates a new ChaincodeSupport instance.
func NewChaincodeSupport(
	config *Config,
	peerAddress string,
	userRunsCC bool,
	caCert []byte,
	certGenerator CertGenerator,
	packageProvider PackageProvider,
	lifecycle Lifecycle,
	aclProvider ACLProvider,
	processor Processor,
	SystemCCProvider sysccprovider.SystemChaincodeProvider,
	platformRegistry *platforms.Registry,
) *ChaincodeSupport {
	cs := &ChaincodeSupport{
		UserRunsCC:       userRunsCC,
		Keepalive:        config.Keepalive,
		ExecuteTimeout:   config.ExecuteTimeout,
		HandlerRegistry:  NewHandlerRegistry(userRunsCC),
		ACLProvider:      aclProvider,
		SystemCCProvider: SystemCCProvider,
		Lifecycle:        lifecycle,
	}

	// Keep TestQueries working
	if !config.TLSEnabled {
		certGenerator = nil
	}

	cs.Runtime = &ContainerRuntime{
		CertGenerator:    certGenerator,
		Processor:        processor,
		CACert:           caCert,
		PeerAddress:      peerAddress,
		PlatformRegistry: platformRegistry,
		CommonEnv: []string{
			"CORE_CHAINCODE_LOGGING_LEVEL=" + config.LogLevel,
			"CORE_CHAINCODE_LOGGING_SHIM=" + config.ShimLogLevel,
			"CORE_CHAINCODE_LOGGING_FORMAT=" + config.LogFormat,
		},
	}

	cs.Launcher = &RuntimeLauncher{
		Runtime:         cs.Runtime,
		Registry:        cs.HandlerRegistry,
		PackageProvider: packageProvider,
		StartupTimeout:  config.StartupTimeout,
	}

	return cs
}

// LaunchForInit bypasses getting the chaincode spec from the LSCC table
// as in the case of v1.0-v1.2 lifecycle, the chaincode will not yet be
// defined in the LSCC table
func (cs *ChaincodeSupport) LaunchInit(cccid *ccprovider.CCContext, spec *pb.ChaincodeDeploymentSpec) error {
	cname := cccid.GetCanonicalName()
	if cs.HandlerRegistry.Handler(cname) != nil {
		return nil
	}

	ccci := ccprovider.DeploymentSpecToChaincodeContainerInfo(spec)
	ccci.Version = cccid.Version

	return cs.Launcher.Launch(ccci)
}

// Launch starts executing chaincode if it is not already running. This method
// blocks until the peer side handler gets into ready state or encounters a fatal
// error. If the chaincode is already running, it simply returns.
func (cs *ChaincodeSupport) Launch(cccid *ccprovider.CCContext, spec *pb.ChaincodeInvocationSpec) error {
	cname := cccid.GetCanonicalName()
	if cs.HandlerRegistry.Handler(cname) != nil {
		return nil
	}

	chaincodeName := spec.GetChaincodeSpec().Name()

	ccci, err := cs.Lifecycle.ChaincodeContainerInfo(cccid.ChainID, chaincodeName)
	if err != nil {
		// TODO: There has to be a better way to do this...
		if cs.UserRunsCC {
			chaincodeLogger.Error(
				"You are attempting to perform an action other than Deploy on Chaincode that is not ready and you are in developer mode. Did you forget to Deploy your chaincode?",
			)
		}

		return errors.Wrapf(err, "[channel %s] failed to get chaincode container info for %s", cccid.ChainID, chaincodeName)
	}

	return cs.Launcher.Launch(ccci)
}

// Stop stops a chaincode if running.
func (cs *ChaincodeSupport) Stop(ccci *ccprovider.ChaincodeContainerInfo) error {
	err := cs.Runtime.Stop(ccci)
	if err != nil {
		return err
	}

	return nil
}

// HandleChaincodeStream implements ccintf.HandleChaincodeStream for all vms to call with appropriate stream
func (cs *ChaincodeSupport) HandleChaincodeStream(stream ccintf.ChaincodeStream) error {
	handler := &Handler{
		Invoker:                    cs,
		DefinitionGetter:           cs.Lifecycle,
		Keepalive:                  cs.Keepalive,
		Registry:                   cs.HandlerRegistry,
		ACLProvider:                cs.ACLProvider,
		TXContexts:                 NewTransactionContexts(),
		ActiveTransactions:         NewActiveTransactions(),
		SystemCCProvider:           cs.SystemCCProvider,
		SystemCCVersion:            util.GetSysCCVersion(),
		InstantiationPolicyChecker: CheckInstantiationPolicyFunc(ccprovider.CheckInstantiationPolicy),
		QueryResponseBuilder:       &QueryResponseGenerator{MaxResultLimit: 100},
		UUIDGenerator:              UUIDGeneratorFunc(util.GenerateUUID),
		LedgerGetter:               peer.Default,
	}

	return handler.ProcessStream(stream)
}

// Register the bidi stream entry point called by chaincode to register with the Peer.
func (cs *ChaincodeSupport) Register(stream pb.ChaincodeSupport_RegisterServer) error {
	return cs.HandleChaincodeStream(stream)
}

// createCCMessage creates a transaction message.
func createCCMessage(messageType pb.ChaincodeMessage_Type, cid string, txid string, cMsg *pb.ChaincodeInput) (*pb.ChaincodeMessage, error) {
	payload, err := proto.Marshal(cMsg)
	if err != nil {
		return nil, err
	}
	ccmsg := &pb.ChaincodeMessage{
		Type:      messageType,
		Payload:   payload,
		Txid:      txid,
		ChannelId: cid,
	}
	return ccmsg, nil
}

// Execute init invokes chaincode and returns the original response.
func (cs *ChaincodeSupport) ExecuteInit(ctxt context.Context, cccid *ccprovider.CCContext, spec *pb.ChaincodeDeploymentSpec) (*pb.Response, *pb.ChaincodeEvent, error) {
	resp, err := cs.InvokeInit(ctxt, cccid, spec)
	return processChaincodeExecutionResult(cccid, resp, err)
}

// Execute invokes chaincode and returns the original response.
func (cs *ChaincodeSupport) Execute(ctxt context.Context, cccid *ccprovider.CCContext, spec *pb.ChaincodeInvocationSpec) (*pb.Response, *pb.ChaincodeEvent, error) {
	resp, err := cs.Invoke(ctxt, cccid, spec)
	return processChaincodeExecutionResult(cccid, resp, err)
}

func processChaincodeExecutionResult(cccid *ccprovider.CCContext, resp *pb.ChaincodeMessage, err error) (*pb.Response, *pb.ChaincodeEvent, error) {
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to execute transaction %s", cccid.TxID)
	}
	if resp == nil {
		return nil, nil, errors.Errorf("nil response from transaction %s", cccid.TxID)
	}

	if resp.ChaincodeEvent != nil {
		resp.ChaincodeEvent.ChaincodeId = cccid.Name
		resp.ChaincodeEvent.TxId = cccid.TxID
	}

	switch resp.Type {
	case pb.ChaincodeMessage_COMPLETED:
		res := &pb.Response{}
		err := proto.Unmarshal(resp.Payload, res)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "failed to unmarshal response for transaction %s", cccid.TxID)
		}
		return res, resp.ChaincodeEvent, nil

	case pb.ChaincodeMessage_ERROR:
		return nil, resp.ChaincodeEvent, errors.Errorf("transaction returned with failure: %s", resp.Payload)

	default:
		return nil, nil, errors.Errorf("unexpected response type %d for transaction %s", resp.Type, cccid.TxID)
	}
}

func (cs *ChaincodeSupport) InvokeInit(ctxt context.Context, cccid *ccprovider.CCContext, spec *pb.ChaincodeDeploymentSpec) (*pb.ChaincodeMessage, error) {
	cctyp := pb.ChaincodeMessage_INIT

	err := cs.LaunchInit(cccid, spec)
	if err != nil {
		return nil, err
	}

	chaincodeSpec := spec.GetChaincodeSpec()
	if chaincodeSpec == nil {
		return nil, errors.New("chaincode spec is nil")
	}

	input := chaincodeSpec.Input
	input.Decorations = cccid.ProposalDecorations
	ccMsg, err := createCCMessage(cctyp, cccid.ChainID, cccid.TxID, input)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to create chaincode message")
	}

	return cs.execute(ctxt, cccid, ccMsg)
}

// Invoke will invoke chaincode and return the message containing the response.
// The chaincode will be launched if it is not already running.
func (cs *ChaincodeSupport) Invoke(ctxt context.Context, cccid *ccprovider.CCContext, spec *pb.ChaincodeInvocationSpec) (*pb.ChaincodeMessage, error) {
	cctyp := pb.ChaincodeMessage_TRANSACTION

	chaincodeSpec := spec.GetChaincodeSpec()
	if chaincodeSpec == nil {
		return nil, errors.New("chaincode spec is nil")
	}

	err := cs.Launch(cccid, spec)
	if err != nil {
		return nil, err
	}

	input := chaincodeSpec.Input
	input.Decorations = cccid.ProposalDecorations
	ccMsg, err := createCCMessage(cctyp, cccid.ChainID, cccid.TxID, input)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to create chaincode message")
	}

	return cs.execute(ctxt, cccid, ccMsg)
}

// execute executes a transaction and waits for it to complete until a timeout value.
func (cs *ChaincodeSupport) execute(ctxt context.Context, cccid *ccprovider.CCContext, msg *pb.ChaincodeMessage) (*pb.ChaincodeMessage, error) {
	cname := cccid.GetCanonicalName()
	chaincodeLogger.Debugf("canonical name: %s", cname)

	handler := cs.HandlerRegistry.Handler(cname)
	if handler == nil {
		chaincodeLogger.Debugf("chaincode is not running: %s", cname)
		return nil, errors.Errorf("unable to invoke chaincode %s", cname)
	}

	ccresp, err := handler.Execute(ctxt, cccid, msg, cs.ExecuteTimeout)
	if err != nil {
		return nil, errors.WithMessage(err, fmt.Sprintf("error sending"))
	}

	return ccresp, nil
}
