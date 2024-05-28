package zcn

import (
	"context"
	"errors"
	"time"

	"github.com/0chain/gosdk/zboxcore/sdk"
)

var (
	batchUploadChan chan sdk.OperationRequest
	workerChan      chan []sdk.OperationRequest
)

func IntiBatchUploadWorkers(ctx context.Context, alloc *sdk.Allocation, waitTime, maxOperations, maxWorkers int) {

	batchUploadChan = make(chan sdk.OperationRequest, maxOperations*maxWorkers)
	workerChan = make(chan []sdk.OperationRequest, maxWorkers)

	for i := 0; i < maxWorkers; i++ {
		go batchUploadWorker(ctx, alloc, workerChan)
	}
	opRequest := make([]sdk.OperationRequest, 0, 5)
	opWaitTime := waitTime
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case op := <-batchUploadChan:
				// process the batch upload or wait for more operations
				opRequest = append(opRequest, op)
				if len(opRequest) == maxOperations || opWaitTime < waitTime/4 {
					workerChan <- opRequest
					opRequest = make([]sdk.OperationRequest, 0, 5)
					opWaitTime = waitTime
					continue
				}
				if len(batchUploadChan) == 0 {
					// wait for more operations
					time.Sleep(time.Duration(opWaitTime) * time.Millisecond)
				} else {
					// consume more operations
					continue
				}
				// if there are no more operations process the batch
				if len(batchUploadChan) == 0 {
					workerChan <- opRequest
					opRequest = make([]sdk.OperationRequest, 0, 5)
					opWaitTime = waitTime
				} else {
					opWaitTime -= (opWaitTime / 2)
				}
			}
		}
	}()

}

func batchUploadWorker(ctx context.Context, alloc *sdk.Allocation, opsChan chan []sdk.OperationRequest) {
	for {
		select {
		case <-ctx.Done():
			return
		case ops := <-opsChan:
			// process the batch upload or wait for more operations
			err := alloc.DoMultiOperation(ops)
			if err != nil {
				if !isSameRootError(err) {
					err = nil
				} else if err == context.Canceled {
					err = errors.New("operation canceled")
				}
			}
			for _, op := range ops {
				op.CancelCauseFunc(err)
			}
		}
	}
}

//Architecture, take max operations from the channel and process time, start with lowest time and keep increasing the time
