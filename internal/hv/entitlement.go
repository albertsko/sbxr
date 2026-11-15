package hv

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Entitlement is the entitlement that a macOS binary needs to use Hypervisor.framework.
const Entitlement = "com.apple.security.hypervisor"

// Values from the Apple code signing format (cs_blobs.h).
const (
	lcCodeSignature              = 0x1d
	csMagicEmbeddedSignature     = 0xfade0cc0
	csMagicEmbeddedEntitlements  = 0xfade7171
	superBlobHeaderSize          = 12
	blobIndexSize                = 8
	blobHeaderSize               = 8
	codeSignatureLoadCommandSize = 16
)

var (
	errNotSigned  = errors.New("binary is not signed")
	errBadSigBlob = errors.New("code signature is malformed")
)

// CheckEntitlement returns nil when the Mach-O binary at path is signed with the hypervisor entitlement.
func CheckEntitlement(path string) error {
	plist, err := entitlements(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	ok, err := plistTrue(plist, Entitlement)
	if err != nil {
		return fmt.Errorf("%s: parse entitlements: %w", path, err)
	}

	if !ok {
		return fmt.Errorf("%s: signed without the %s entitlement", path, Entitlement)
	}

	return nil
}

// entitlements returns the entitlements plist embedded in the code signature, or nil when it has none.
func entitlements(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open binary: %w", err)
	}
	defer func() { _ = f.Close() }()

	file, err := macho.NewFile(f)
	if err != nil {
		return nil, fmt.Errorf("read Mach-O: %w", err)
	}

	sig, err := codeSignature(file, f)
	if err != nil {
		return nil, err
	}

	return entitlementsBlob(sig)
}

func codeSignature(file *macho.File, r io.ReaderAt) ([]byte, error) {
	for _, load := range file.Loads {
		raw := load.Raw()
		if len(raw) < codeSignatureLoadCommandSize || file.ByteOrder.Uint32(raw) != lcCodeSignature {
			continue
		}

		offset := file.ByteOrder.Uint32(raw[8:])
		size := file.ByteOrder.Uint32(raw[12:])
		sig := make([]byte, size)

		_, err := r.ReadAt(sig, int64(offset))
		if err != nil {
			return nil, fmt.Errorf("read code signature: %w", err)
		}

		return sig, nil
	}

	return nil, errNotSigned
}

// entitlementsBlob walks the big-endian SuperBlob index of an embedded signature.
func entitlementsBlob(sig []byte) ([]byte, error) {
	be := binary.BigEndian
	if len(sig) < superBlobHeaderSize || be.Uint32(sig) != csMagicEmbeddedSignature {
		return nil, errBadSigBlob
	}

	count := uint64(be.Uint32(sig[8:]))
	if superBlobHeaderSize+count*blobIndexSize > uint64(len(sig)) {
		return nil, errBadSigBlob
	}

	for i := range count {
		index := superBlobHeaderSize + i*blobIndexSize

		offset := uint64(be.Uint32(sig[index+4:]))
		if offset+blobHeaderSize > uint64(len(sig)) {
			return nil, errBadSigBlob
		}

		if be.Uint32(sig[offset:]) != csMagicEmbeddedEntitlements {
			continue
		}

		end := offset + uint64(be.Uint32(sig[offset+4:]))
		if end < offset+blobHeaderSize || end > uint64(len(sig)) {
			return nil, errBadSigBlob
		}

		return sig[offset+blobHeaderSize : end], nil
	}

	return nil, nil
}

// plistTrue reports whether the plist has key set to true.
func plistTrue(plist []byte, key string) (bool, error) {
	dec := xml.NewDecoder(bytes.NewReader(plist))
	inKey, matched := false, false

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return false, nil
		}

		if err != nil {
			return false, fmt.Errorf("decode plist: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if matched {
				return t.Name.Local == "true", nil
			}

			inKey = t.Name.Local == "key"
		case xml.CharData:
			if inKey {
				matched = strings.TrimSpace(string(t)) == key
			}
		case xml.EndElement:
			inKey = false
		}
	}
}
