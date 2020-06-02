package byzantine

import (
	"github.com/oasisprotocol/oasis-core/go/consensus/tendermint/service"
	epochtime "github.com/oasisprotocol/oasis-core/go/epochtime/api"
)

func epochtimeWaitForEpoch(svc service.TendermintService, epoch epochtime.EpochTime) error {
	ch, sub := svc.EpochTime().WatchEpochs()
	defer sub.Close()

	for {
		currentEpoch := <-ch
		if currentEpoch >= epoch {
			return nil
		}
	}
}
