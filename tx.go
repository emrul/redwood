package redwood

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/brynbellomy/redwood/tree"
	"github.com/brynbellomy/redwood/types"
)

var (
	GenesisTxID = types.IDFromString("genesis")
	EmptyHash   = types.Hash{}
)

const (
	KeypathSeparator = "."
)

type Tx struct {
	ID         types.ID        `json:"id"`
	Parents    []types.ID      `json:"parents"`
	From       types.Address   `json:"from"`
	Sig        types.Signature `json:"sig,omitempty"`
	URL        string          `json:"url"`
	Patches    []Patch         `json:"patches"`
	Recipients []types.Address `json:"recipients,omitempty"`
	Checkpoint bool            `json:"checkpoint"` // @@TODO: probably not ideal

	Valid bool       `json:"valid"`
	hash  types.Hash `json:"-"`
}

func (tx Tx) Hash() types.Hash {
	if tx.hash == types.EmptyHash {
		var txBytes []byte

		txBytes = append(txBytes, tx.ID[:]...)

		for i := range tx.Parents {
			txBytes = append(txBytes, tx.Parents[i][:]...)
		}

		txBytes = append(txBytes, []byte(tx.URL)...)

		for i := range tx.Patches {
			txBytes = append(txBytes, []byte(tx.Patches[i].String())...)
		}

		for i := range tx.Recipients {
			txBytes = append(txBytes, tx.Recipients[i][:]...)
		}

		tx.hash = types.HashBytes(txBytes)
	}

	return tx.hash
}

func (tx Tx) IsPrivate() bool {
	return len(tx.Recipients) > 0
}

func PrivateRootKeyForRecipients(recipients []types.Address) string {
	var bs []byte
	for _, r := range recipients {
		bs = append(bs, r[:]...)
	}
	return "private-" + types.HashBytes(bs).Hex()
}

func (tx Tx) PrivateRootKey() string {
	return PrivateRootKeyForRecipients(tx.Recipients)
}

type Patch struct {
	Keypath tree.Keypath
	Range   *tree.Range
	Val     interface{}
}

type Range struct {
	Start int64
	End   int64
}

func (p Patch) String() string {
	parts := p.Keypath.Parts()
	var keypathParts []string
	for _, key := range parts {
		if bytes.IndexByte(key, '.') > -1 {
			keypathParts = append(keypathParts, `["`+string(key)+`"]`)
		} else {
			keypathParts = append(keypathParts, KeypathSeparator+string(key))
		}
	}
	s := strings.Join(keypathParts, "")

	if p.Range != nil {
		s += fmt.Sprintf("[%v:%v]", p.Range[0], p.Range[1])
	}

	val, err := json.Marshal(p.Val)
	if err != nil {
		panic(err)
	}

	s += " = " + string(val)

	return s
}

func (p Patch) Copy() Patch {
	return Patch{
		Keypath: p.Keypath.Copy(),
		Range:   p.Range.Copy(),
		Val:     DeepCopyJSValue(p.Val), // @@TODO?
	}
}

func (p *Patch) UnmarshalJSON(bs []byte) error {
	var err error
	var s string
	err = json.Unmarshal(bs, &s)
	if err != nil {
		return err
	}
	*p, err = ParsePatch([]byte(s))
	return err
}

func (p Patch) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

func (r *Range) Copy() *Range {
	if r == nil {
		return nil
	}
	return &Range{r.Start, r.End}
}
