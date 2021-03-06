package redwood

import (
	"encoding/json"
	goerrors "errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/crypto/sha3"

	"github.com/brynbellomy/redwood/types"
)

type RefStore interface {
	Object(hash types.Hash) (io.ReadCloser, int64, error)
	StoreObject(reader io.ReadCloser, contentType string) (types.Hash, error)
	HaveObject(hash types.Hash) bool
	AllHashes() ([]types.Hash, error)
}

type refStore struct {
	rootPath   string
	fileMu     sync.Mutex
	metadataMu sync.Mutex
}

func NewRefStore(rootPath string) RefStore {
	return &refStore{rootPath: rootPath}
}

func (s *refStore) ensureRootPath() error {
	return os.MkdirAll(s.rootPath, 0755)
}

func (s *refStore) Object(hash types.Hash) (io.ReadCloser, int64, error) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	err := s.ensureRootPath()
	if err != nil {
		return nil, 0, err
	}

	filename := filepath.Join(s.rootPath, "ref-"+hash.String())
	stat, err := os.Stat(filename)
	if err != nil {
		return nil, 0, err
	}

	f, err := os.Open(filename)
	if err != nil {
		return nil, 0, err
	}

	//contentType, err := s.contentType(hash)
	//if err != nil {
	//    return nil, "", err
	//}

	return f, stat.Size(), nil
}

func (s *refStore) StoreObject(reader io.ReadCloser, contentType string) (h types.Hash, err error) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	defer annotate(&err, "refStore.StoreObject")

	err = s.ensureRootPath()
	if err != nil {
		return types.Hash{}, err
	}

	tmpFile, err := ioutil.TempFile(s.rootPath, "temp-")
	if err != nil {
		return types.Hash{}, err
	}
	defer func() {
		closeErr := tmpFile.Close()
		if closeErr != nil && !goerrors.Is(closeErr, os.ErrClosed) {
			err = closeErr
		}
	}()

	hasher := sha3.NewLegacyKeccak256()
	tee := io.TeeReader(reader, hasher)

	_, err = io.Copy(tmpFile, tee)
	if err != nil {
		return types.Hash{}, err
	}

	bs := hasher.Sum(nil)
	var hash types.Hash
	copy(hash[:], bs)

	err = tmpFile.Close()
	if err != nil {
		return types.Hash{}, err
	}

	err = os.Rename(tmpFile.Name(), filepath.Join(s.rootPath, "ref-"+hash.String()))
	if err != nil {
		return hash, err
	}

	err = s.setContentType(hash, contentType)
	if err != nil {
		return hash, err
	}

	return hash, nil
}

func (s *refStore) HaveObject(hash types.Hash) bool {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	return fileExists(filepath.Join(s.rootPath, "ref-"+hash.String()))
}

func (s *refStore) contentType(hash types.Hash) (string, error) {
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()

	f, err := os.Open(filepath.Join(s.rootPath, "metadata.json"))
	if err != nil {
		return "", err
	}
	defer f.Close()

	var metadata map[string]interface{}
	err = json.NewDecoder(f).Decode(&metadata)
	if err != nil {
		return "", err
	}

	contentType, exists := getString(metadata, []string{hash.String(), "Content-Type"})
	if !exists {
		return "", nil
	}

	return contentType, nil
}

func (s *refStore) setContentType(hash types.Hash, contentType string) error {
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()

	f, err := os.OpenFile(filepath.Join(s.rootPath, "metadata.json"), os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		return err
	}
	defer f.Close()

	var metadata map[string]interface{}
	err = json.NewDecoder(f).Decode(&metadata)
	if errors.Cause(err) == io.EOF {
		metadata = make(map[string]interface{})
	} else if err != nil {
		return err
	}

	setValueAtKeypath(metadata, []string{hash.String(), "Content-Type"}, contentType, true)

	_, err = f.Seek(0, 0)
	if err != nil {
		return err
	}

	err = json.NewEncoder(f).Encode(metadata)
	if err != nil {
		return err
	}
	return nil
}

func (s *refStore) AllHashes() ([]types.Hash, error) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	err := s.ensureRootPath()
	if err != nil {
		return nil, err
	}

	matches, err := filepath.Glob(filepath.Join(s.rootPath, "ref-*"))
	if err != nil {
		return nil, err
	}

	var refHashes []types.Hash
	for _, match := range matches {
		hash, err := types.HashFromHex(filepath.Base(match)[4:])
		if err != nil {
			// ignore (@@TODO: delete?  notify?)
			continue
		}
		refHashes = append(refHashes, hash)
	}
	return refHashes, nil
}
