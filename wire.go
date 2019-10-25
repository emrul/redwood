package redwood

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"

	"github.com/pkg/errors"
)

type Msg struct {
	Type    MsgType     `json:"type"`
	Payload interface{} `json:"payload"`
}

type MsgType string

const (
	MsgType_Subscribe             MsgType = "subscribe"
	MsgType_Unsubscribe           MsgType = "unsubscribe"
	MsgType_Put                   MsgType = "put"
	MsgType_Private               MsgType = "private"
	MsgType_Ack                   MsgType = "ack"
	MsgType_Error                 MsgType = "error"
	MsgType_VerifyAddress         MsgType = "verify address"
	MsgType_VerifyAddressResponse MsgType = "verify address response"
)

type VerifyAddressResponse struct {
	Signature           []byte `json:"signature"`
	EncryptingPublicKey []byte `json:"encryptingPublicKey"`
}

type EncryptedTx struct {
	TxHash           Hash   `json:"txHash"`
	EncryptedPayload []byte `json:"encryptedPayload"`
	SenderPublicKey  []byte `json:"senderPublicKey"`
}

func WriteMsg(w io.Writer, msg Msg) error {
	bs, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	buflen := uint64(len(bs))

	err = WriteUint64(w, buflen)
	if err != nil {
		return err
	}
	n, err := io.Copy(w, bytes.NewReader(bs))
	if err != nil {
		return err
	} else if n != int64(buflen) {
		return errors.New("WriteMsg: could not write entire packet")
	}
	return nil
}

func ReadMsg(r io.Reader, msg *Msg) error {
	size, err := ReadUint64(r)
	if err != nil {
		return err
	}

	buf := &bytes.Buffer{}
	_, err = io.CopyN(buf, r, int64(size))
	if err != nil {
		return err
	}

	err = json.NewDecoder(buf).Decode(msg)
	if err != nil {
		return err
	}
	return nil
}

func ReadUint64(r io.Reader) (uint64, error) {
	buf := make([]byte, 8)
	_, err := io.ReadFull(r, buf)
	if err == io.EOF {
		return 0, err
	} else if err != nil {
		return 0, errors.Wrap(err, "ReadUint64")
	}
	return binary.LittleEndian.Uint64(buf), nil
}

func WriteUint64(w io.Writer, n uint64) error {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, n)
	written, err := w.Write(buf)
	if err != nil {
		return err
	} else if written < 8 {
		return errors.Wrap(err, "WriteUint64")
	}
	return nil
}

func (msg *Msg) UnmarshalJSON(bs []byte) error {
	var m struct {
		Type         string          `json:"type"`
		PayloadBytes json.RawMessage `json:"payload"`
	}

	err := json.Unmarshal(bs, &m)
	if err != nil {
		return err
	}

	msg.Type = MsgType(m.Type)

	switch msg.Type {
	case MsgType_Subscribe:
		url := string(m.PayloadBytes)
		msg.Payload = url[1 : len(url)-1] // remove quotes

	case MsgType_Put:
		var tx Tx
		err := json.Unmarshal(m.PayloadBytes, &tx)
		if err != nil {
			return err
		}
		msg.Payload = tx

	case MsgType_Ack:
		var hash Hash
		bs := []byte(m.PayloadBytes[1 : len(m.PayloadBytes)-1]) // remove quotes
		copy(hash[:], bs)
		msg.Payload = hash

	case MsgType_Private:
		var ep EncryptedTx
		err := json.Unmarshal(m.PayloadBytes, &ep)
		if err != nil {
			return err
		}
		msg.Payload = ep

	case MsgType_VerifyAddress:
		msg.Payload = []byte(m.PayloadBytes)

	case MsgType_VerifyAddressResponse:
		var resp VerifyAddressResponse
		err := json.Unmarshal([]byte(m.PayloadBytes), &resp)
		if err != nil {
			return err
		}

		msg.Payload = resp

	default:
		return errors.New("bad msg")
	}

	return nil
}
