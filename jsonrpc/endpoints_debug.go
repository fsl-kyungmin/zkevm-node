package jsonrpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/client"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/types"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime/fakevm"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime/instrumentation"
	"github.com/ethereum/go-ethereum/common"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v4"
)

var defaultTraceConfig = &traceConfig{
	DisableStorage:   false,
	DisableStack:     false,
	EnableMemory:     false,
	EnableReturnData: false,
	Tracer:           nil,
}

// DebugEndpoints is the debug jsonrpc endpoint
type DebugEndpoints struct {
	state types.StateInterface
	txMan dbTxManager
}

type traceConfig struct {
	DisableStorage   bool            `json:"disableStorage"`
	DisableStack     bool            `json:"disableStack"`
	EnableMemory     bool            `json:"enableMemory"`
	EnableReturnData bool            `json:"enableReturnData"`
	Tracer           *string         `json:"tracer"`
	TracerConfig     json.RawMessage `json:"tracerConfig"`
}

// StructLogRes represents the debug trace information for each opcode
type StructLogRes struct {
	Pc            uint64             `json:"pc"`
	Op            string             `json:"op"`
	Gas           uint64             `json:"gas"`
	GasCost       uint64             `json:"gasCost"`
	Depth         int                `json:"depth"`
	Error         string             `json:"error,omitempty"`
	Stack         *[]types.ArgBig    `json:"stack,omitempty"`
	Memory        *[]string          `json:"memory,omitempty"`
	Storage       *map[string]string `json:"storage,omitempty"`
	RefundCounter uint64             `json:"refund,omitempty"`
}

type traceTransactionResponse struct {
	Gas         uint64         `json:"gas"`
	Failed      bool           `json:"failed"`
	ReturnValue interface{}    `json:"returnValue"`
	StructLogs  []StructLogRes `json:"structLogs"`
}

type traceBlockTransactionResponse struct {
	Result interface{} `json:"result"`
}

type traceBatchTransactionResponse struct {
	TxHash common.Hash `json:"txHash"`
	Result interface{} `json:"result"`
}

// TraceTransaction creates a response for debug_traceTransaction request.
// See https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtracetransaction
func (d *DebugEndpoints) TraceTransaction(hash types.ArgHash, cfg *traceConfig) (interface{}, types.Error) {
	return d.txMan.NewDbTxScope(d.state, func(ctx context.Context, dbTx pgx.Tx) (interface{}, types.Error) {
		return d.buildTraceTransaction(ctx, hash.Hash(), cfg, dbTx)
	})
}

// TraceBlockByNumber creates a response for debug_traceBlockByNumber request.
// See https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtraceblockbynumber
func (d *DebugEndpoints) TraceBlockByNumber(number types.BlockNumber, cfg *traceConfig) (interface{}, types.Error) {
	return d.txMan.NewDbTxScope(d.state, func(ctx context.Context, dbTx pgx.Tx) (interface{}, types.Error) {
		blockNumber, rpcErr := number.GetNumericBlockNumber(ctx, d.state, dbTx)
		if rpcErr != nil {
			return nil, rpcErr
		}

		block, err := d.state.GetL2BlockByNumber(ctx, blockNumber, dbTx)
		if errors.Is(err, state.ErrNotFound) {
			return nil, types.NewRPCError(types.DefaultErrorCode, fmt.Sprintf("block #%d not found", blockNumber))
		} else if err == state.ErrNotFound {
			return rpcErrorResponse(types.DefaultErrorCode, "failed to get block by number", err)
		}

		traces, rpcErr := d.buildTraceBlock(ctx, block.Transactions(), cfg, dbTx)
		if err != nil {
			return nil, rpcErr
		}

		return traces, nil
	})
}

// TraceBlockByHash creates a response for debug_traceBlockByHash request.
// See https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtraceblockbyhash
func (d *DebugEndpoints) TraceBlockByHash(hash types.ArgHash, cfg *traceConfig) (interface{}, types.Error) {
	return d.txMan.NewDbTxScope(d.state, func(ctx context.Context, dbTx pgx.Tx) (interface{}, types.Error) {
		block, err := d.state.GetL2BlockByHash(ctx, hash.Hash(), dbTx)
		if errors.Is(err, state.ErrNotFound) {
			return nil, types.NewRPCError(types.DefaultErrorCode, fmt.Sprintf("block %s not found", hash.Hash().String()))
		} else if err == state.ErrNotFound {
			return rpcErrorResponse(types.DefaultErrorCode, "failed to get block by hash", err)
		}

		traces, rpcErr := d.buildTraceBlock(ctx, block.Transactions(), cfg, dbTx)
		if err != nil {
			return nil, rpcErr
		}

		return traces, nil
	})
}

// TraceBatchByNumber creates a response for debug_traceBatchByNumber request.
// this endpoint tries to help clients to get traces at once for all the transactions
// attached to the same batch.
//
// IMPORTANT: in order to take advantage of the infrastructure automatically scaling,
// instead of parallelizing the trace transaction internally and pushing all the load
// to a single jRPC and Executor instance, the code will redirect the trace transaction
// requests to the same url, making them external calls, so we can process in parallel
// with multiple jRPC and Executor instances.
//
// the request flow will work as follows:
// -> user do a trace batch request
// -> jRPC balancer picks a jRPC server to handle the trace batch request
// -> picked jRPC sends parallel trace transaction requests for each transaction in the batch
// -> jRPC balancer sends each request to a different jRPC to handle the trace transaction requests
// -> picked jRPC server group trace transaction responses from other jRPC servers
// -> picked jRPC respond the initial request to the user with all the tx traces
func (d *DebugEndpoints) TraceBatchByNumber(httpRequest *http.Request, number types.BatchNumber, cfg *traceConfig) (interface{}, types.Error) {
	// timeout is the maximum time the code will wait for all the
	// traces to be loaded from the jRPC and added to the responses
	const timeout = 10 * time.Minute

	// the size of the buffer defines
	// how many txs it will process in parallel.
	const bufferSize = 10

	// builds the url of the jRPC server from the data found in the httpRequest
	scheme := "https"
	if httpRequest.URL.Scheme != "" {
		scheme = httpRequest.URL.Scheme
	}
	u := url.URL{
		Scheme: scheme,
		Host:   httpRequest.Host,
		Path:   httpRequest.URL.Path,
	}
	rpcURL := u.String()

	return d.txMan.NewDbTxScope(d.state, func(ctx context.Context, dbTx pgx.Tx) (interface{}, types.Error) {
		batchNumber, rpcErr := number.GetNumericBatchNumber(ctx, d.state, dbTx)
		if rpcErr != nil {
			return nil, rpcErr
		}

		batch, err := d.state.GetBatchByNumber(ctx, batchNumber, dbTx)
		if errors.Is(err, state.ErrNotFound) {
			return nil, types.NewRPCError(types.DefaultErrorCode, fmt.Sprintf("batch #%d not found", batchNumber))
		} else if err == state.ErrNotFound {
			return rpcErrorResponse(types.DefaultErrorCode, "failed to get batch by number", err)
		}

		txs, err := d.state.GetTransactionsByBatchNumber(ctx, batch.BatchNumber, dbTx)
		if !errors.Is(err, state.ErrNotFound) && err != nil {
			return rpcErrorResponse(types.DefaultErrorCode, fmt.Sprintf("couldn't load batch txs from state by number %v to create the traces", batchNumber), err)
		}

		receipts := make([]ethTypes.Receipt, 0, len(txs))
		for _, tx := range txs {
			receipt, err := d.state.GetTransactionReceipt(ctx, tx.Hash(), dbTx)
			if err != nil {
				return rpcErrorResponse(types.DefaultErrorCode, fmt.Sprintf("couldn't load receipt for tx %v to get trace", tx.Hash().String()), err)
			}
			receipts = append(receipts, *receipt)
		}

		requests := make(chan (ethTypes.Receipt), bufferSize)

		wg := sync.WaitGroup{}
		wg.Add(len(receipts))
		responses := make(chan (traceBatchTransactionResponse), len(receipts))

		// gets the trace from the jRPC and adds it to the responses
		loadTraceByTxHash := func(receipt ethTypes.Receipt) {
			defer wg.Done()
			res, err := client.JSONRPCCall(rpcURL, "debug_traceTransaction", receipt.TxHash.String(), cfg)
			if err != nil {
				log.Errorf("failed to get tx trace from remote jRPC server %v, err: %v", rpcURL, err)
				return
			}

			if res.Error != nil {
				log.Errorf("tx trace error returned from remote jRPC server %v, %v %v", rpcURL, res.Error.Code, res.Error.Message)
				return
			}

			// add to the responses
			responses <- traceBatchTransactionResponse{
				TxHash: receipt.TxHash,
				Result: res.Result,
			}
		}

		// goes through the buffer and loads the trace
		// by all the transactions added in the buffer
		// then add the results to the responses map
		go func() {
			index := uint(0)
			for req := range requests {
				go loadTraceByTxHash(req)
				index++
			}
		}()

		// add receipts to the buffer
		for _, receipt := range receipts {
			requests <- receipt
		}

		// wait the traces to be loaded
		if waitTimeout(&wg, timeout) {
			return rpcErrorResponse(types.DefaultErrorCode, fmt.Sprintf("failed to get traces for batch %v: timeout reached", batchNumber), nil)
		}

		close(requests)
		close(responses)

		// build the batch trace response array
		traces := make([]traceBatchTransactionResponse, 0, len(receipts))
		for response := range responses {
			traces = append(traces, response)
		}
		return traces, nil
	})
}

func (d *DebugEndpoints) buildTraceBlock(ctx context.Context, txs []*ethTypes.Transaction, cfg *traceConfig, dbTx pgx.Tx) (interface{}, types.Error) {
	traces := []traceBlockTransactionResponse{}
	for _, tx := range txs {
		traceTransaction, err := d.buildTraceTransaction(ctx, tx.Hash(), cfg, dbTx)
		if err != nil {
			errMsg := fmt.Sprintf("failed to get trace for transaction %v", tx.Hash().String())
			return rpcErrorResponse(types.DefaultErrorCode, errMsg, err)
		}
		traceBlockTransaction := traceBlockTransactionResponse{
			Result: traceTransaction,
		}
		traces = append(traces, traceBlockTransaction)
	}

	return traces, nil
}

func (d *DebugEndpoints) buildTraceTransaction(ctx context.Context, hash common.Hash, cfg *traceConfig, dbTx pgx.Tx) (interface{}, types.Error) {
	traceCfg := cfg
	if traceCfg == nil {
		traceCfg = defaultTraceConfig
	}

	// check tracer
	if traceCfg.Tracer != nil && *traceCfg.Tracer != "" && !isBuiltInTracer(*traceCfg.Tracer) && !isJSCustomTracer(*traceCfg.Tracer) {
		return rpcErrorResponse(types.DefaultErrorCode, "invalid tracer", nil)
	}

	stateTraceConfig := state.TraceConfig{
		DisableStack:     traceCfg.DisableStack,
		DisableStorage:   traceCfg.DisableStorage,
		EnableMemory:     traceCfg.EnableMemory,
		EnableReturnData: traceCfg.EnableReturnData,
		Tracer:           traceCfg.Tracer,
		TracerConfig:     traceCfg.TracerConfig,
	}
	result, err := d.state.DebugTransaction(ctx, hash, stateTraceConfig, dbTx)
	if errors.Is(err, state.ErrNotFound) {
		return rpcErrorResponse(types.DefaultErrorCode, "transaction not found", nil)
	} else if err != nil {
		const errorMessage = "failed to get trace"
		log.Errorf("%v: %v", errorMessage, err)
		return nil, types.NewRPCError(types.DefaultErrorCode, errorMessage)
	}

	// if a tracer was specified, then return the trace result
	if stateTraceConfig.Tracer != nil && *stateTraceConfig.Tracer != "" && len(result.ExecutorTraceResult) > 0 {
		return result.ExecutorTraceResult, nil
	}

	receipt, err := d.state.GetTransactionReceipt(ctx, hash, dbTx)
	if err != nil {
		const errorMessage = "failed to tx receipt"
		log.Errorf("%v: %v", errorMessage, err)
		return nil, types.NewRPCError(types.DefaultErrorCode, errorMessage)
	}

	failed := receipt.Status == ethTypes.ReceiptStatusFailed
	var returnValue interface{}
	if stateTraceConfig.EnableReturnData {
		returnValue = common.Bytes2Hex(result.ReturnValue)
	}

	structLogs := d.buildStructLogs(result.StructLogs, *traceCfg)

	resp := traceTransactionResponse{
		Gas:         result.GasUsed,
		Failed:      failed,
		ReturnValue: returnValue,
		StructLogs:  structLogs,
	}

	return resp, nil
}

func (d *DebugEndpoints) buildStructLogs(stateStructLogs []instrumentation.StructLog, cfg traceConfig) []StructLogRes {
	structLogs := make([]StructLogRes, 0, len(stateStructLogs))
	memory := fakevm.NewMemory()
	for _, structLog := range stateStructLogs {
		errRes := ""
		if structLog.Err != nil {
			errRes = structLog.Err.Error()
		}

		op := structLog.Op
		if op == "SHA3" {
			op = "KECCAK256"
		} else if op == "STOP" && structLog.Pc == 0 {
			// this stop is generated for calls with single
			// step(no depth increase) and must be ignored
			continue
		}

		structLogRes := StructLogRes{
			Pc:            structLog.Pc,
			Op:            op,
			Gas:           structLog.Gas,
			GasCost:       structLog.GasCost,
			Depth:         structLog.Depth,
			Error:         errRes,
			RefundCounter: structLog.RefundCounter,
		}

		if !cfg.DisableStack {
			stack := make([]types.ArgBig, 0, len(structLog.Stack))
			for _, stackItem := range structLog.Stack {
				if stackItem != nil {
					stack = append(stack, types.ArgBig(*stackItem))
				}
			}
			structLogRes.Stack = &stack
		}

		if cfg.EnableMemory {
			memory.Resize(uint64(structLog.MemorySize))
			if len(structLog.Memory) > 0 {
				memory.Set(uint64(structLog.MemoryOffset), uint64(len(structLog.Memory)), structLog.Memory)
			}

			if structLog.MemorySize > 0 {
				// Populate the structLog memory
				structLog.Memory = memory.Data()

				// Convert memory to string array
				const memoryChunkSize = 32
				memoryArray := make([]string, 0, len(structLog.Memory))

				for i := 0; i < len(structLog.Memory); i = i + memoryChunkSize {
					slice32Bytes := make([]byte, memoryChunkSize)
					copy(slice32Bytes, structLog.Memory[i:i+memoryChunkSize])
					memoryStringItem := hex.EncodeToString(slice32Bytes)
					memoryArray = append(memoryArray, memoryStringItem)
				}

				structLogRes.Memory = &memoryArray
			} else {
				memory = fakevm.NewMemory()
				structLogRes.Memory = &[]string{}
			}
		}

		if !cfg.DisableStorage && len(structLog.Storage) > 0 {
			storage := make(map[string]string, len(structLog.Storage))
			for storageKey, storageValue := range structLog.Storage {
				k := hex.EncodeToString(storageKey.Bytes())
				v := hex.EncodeToString(storageValue.Bytes())
				storage[k] = v
			}
			structLogRes.Storage = &storage
		}

		structLogs = append(structLogs, structLogRes)
	}
	return structLogs
}

// isBuiltInTracer checks if the tracer is one of the
// built-in tracers
func isBuiltInTracer(tracer string) bool {
	// built-in tracers
	switch tracer {
	case "callTracer", "4byteTracer", "prestateTracer", "noopTracer":
		return true
	default:
		return false
	}
}

// isJSCustomTracer checks if the tracer contains the
// functions result and fault which are required for a custom tracer
// https://geth.ethereum.org/docs/developers/evm-tracing/custom-tracer
func isJSCustomTracer(tracer string) bool {
	return strings.Contains(tracer, "result") && strings.Contains(tracer, "fault")
}

// waitTimeout waits for the waitGroup for the specified max timeout.
// Returns true if waiting timed out.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		return false // completed normally
	case <-time.After(timeout):
		return true // timed out
	}
}
