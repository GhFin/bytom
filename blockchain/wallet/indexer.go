package wallet

import (
	"encoding/json"

	log "github.com/sirupsen/logrus"
	"github.com/tendermint/tmlibs/db"

	"github.com/bytom/crypto/sha3pool"
	"github.com/bytom/errors"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/legacy"
	"github.com/bytom/blockchain/account"
)

type rawOutput struct {
	OutputID bc.Hash
	bc.AssetAmount
	ControlProgram []byte
	txHash         bc.Hash
	outputIndex    uint32
	sourceID       bc.Hash
	sourcePos      uint64
	refData        bc.Hash
}

type accountOutput struct {
	rawOutput
	AccountID string
	keyIndex  uint64
	change    bool
}

//ReverseAccountUTXOs process the invalid blocks when orphan block rollback
func (w *Wallet) ReverseAccountUTXOs(batch *db.Batch, b *legacy.Block) {
	var err error

	//unknow how many spent and retire outputs
	reverseOuts := make([]*rawOutput, 0)

	//handle spent UTXOs
	for _, tx := range b.Transactions {
		for _, inpID := range tx.Tx.InputIDs {
			//spend and retire
			sp, err := tx.Spend(inpID)
			if err != nil {
				continue
			}

			resOut, ok := tx.Entries[*sp.SpentOutputId].(*bc.Output)
			if !ok {
				continue
			}

			out := &rawOutput{
				OutputID:       *sp.SpentOutputId,
				AssetAmount:    *resOut.Source.Value,
				ControlProgram: resOut.ControlProgram.Code,
				txHash:         tx.ID,
				sourceID:       *resOut.Source.Ref,
				sourcePos:      resOut.Source.Position,
				refData:        *resOut.Data,
			}
			reverseOuts = append(reverseOuts, out)
		}
	}

	accOuts := w.loadAccountInfo(reverseOuts)
	if err = w.upsertConfirmedAccountOutputs(accOuts, b, batch); err != nil {
		log.WithField("err", err).Error("reversing account spent and retire outputs")
		return
	}

	//handle new UTXOs
	for _, tx := range b.Transactions {
		for j := range tx.Outputs {
			resOutID := tx.ResultIds[j]
			if _, ok := tx.Entries[*resOutID].(*bc.Output); !ok {
				//retirement
				continue
			}
			//delete new UTXOs
			(*batch).Delete(account.AccountUTXOKey(string(resOutID.Bytes())))
		}
	}

}

//BuildAccountUTXOs process valid blocks to build account unspent outputs db
func (w *Wallet) BuildAccountUTXOs(batch *db.Batch, b *legacy.Block) {
	var err error

	//handle spent UTXOs
	delOutputIDs := prevoutDBKeys(b.Transactions...)
	for _, delOutputID := range delOutputIDs {
		(*batch).Delete(account.AccountUTXOKey(string(delOutputID.Bytes())))
	}

	//handle new UTXOs
	outs := make([]*rawOutput, 0, len(b.Transactions))
	for _, tx := range b.Transactions {
		for j, out := range tx.Outputs {
			resOutID := tx.ResultIds[j]
			resOut, ok := tx.Entries[*resOutID].(*bc.Output)
			if !ok {
				continue
			}
			out := &rawOutput{
				OutputID:       *tx.OutputID(j),
				AssetAmount:    out.AssetAmount,
				ControlProgram: out.ControlProgram,
				txHash:         tx.ID,
				outputIndex:    uint32(j),
				sourceID:       *resOut.Source.Ref,
				sourcePos:      resOut.Source.Position,
				refData:        *resOut.Data,
			}
			outs = append(outs, out)
		}
	}
	accOuts := w.loadAccountInfo(outs)

	if err = w.upsertConfirmedAccountOutputs(accOuts, b, batch); err != nil {
		log.WithField("err", err).Error("building new account outputs")
		return
	}
}

func prevoutDBKeys(txs ...*legacy.Tx) (outputIDs []bc.Hash) {
	for _, tx := range txs {
		for _, inpID := range tx.Tx.InputIDs {
			if sp, err := tx.Spend(inpID); err == nil {
				outputIDs = append(outputIDs, *sp.SpentOutputId)
			}
		}
	}
	return
}

// loadAccountInfo turns a set of output IDs into a set of
// outputs by adding account annotations.  Outputs that can't be
// annotated are excluded from the result.
func (w *Wallet) loadAccountInfo(outs []*rawOutput) []*accountOutput {
	outsByScript := make(map[string][]*rawOutput, len(outs))
	for _, out := range outs {
		scriptStr := string(out.ControlProgram)
		outsByScript[scriptStr] = append(outsByScript[scriptStr], out)
	}

	result := make([]*accountOutput, 0, len(outs))
	cp := account.CtrlProgram{}

	var hash [32]byte
	for s := range outsByScript {
		sha3pool.Sum256(hash[:], []byte(s))
		bytes := w.DB.Get(account.AccountCPKey(hash))
		if bytes == nil {
			continue
		}

		err := json.Unmarshal(bytes, &cp)
		if err != nil {
			continue
		}

		isExist := w.DB.Get(account.AccountKey(cp.AccountID))
		if isExist == nil {
			continue
		}

		for _, out := range outsByScript[s] {
			newOut := &accountOutput{
				rawOutput: *out,
				AccountID: cp.AccountID,
				keyIndex:  cp.KeyIndex,
				change:    cp.Change,
			}
			result = append(result, newOut)
		}
	}

	return result
}

// upsertConfirmedAccountOutputs records the account data for confirmed utxos.
// If the account utxo already exists (because it's from a local tx), the
// block confirmation data will in the row will be updated.
func (w *Wallet) upsertConfirmedAccountOutputs(outs []*accountOutput, block *legacy.Block, batch *db.Batch) error {
	var u *account.UTXO

	for _, out := range outs {
		u = &account.UTXO{OutputID: out.OutputID.Bytes(),
			AssetID:      out.AssetId.Bytes(),
			Amount:       out.Amount,
			AccountID:    out.AccountID,
			ProgramIndex: out.keyIndex,
			Program:      out.ControlProgram,
			SourceID:     out.sourceID.Bytes(),
			SourcePos:    out.sourcePos,
			RefData:      out.refData.Bytes(),
			Change:       out.change}

		rawUTXO, err := json.Marshal(u)
		if err != nil {
			return errors.Wrap(err, "failed marshal accountutxo")
		}

		(*batch).Set(account.AccountUTXOKey(string(u.OutputID)), rawUTXO)
	}
	return nil
}

