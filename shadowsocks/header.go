package shadowsocks

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"time"

	"github.com/Shadowsocks-NET/outline-ss-server/slicepool"
	"github.com/Shadowsocks-NET/outline-ss-server/socks"
)

const (
	HeaderTypeClientStream = 0
	HeaderTypeServerStream = 1

	HeaderTypeClientPacket = 0
	HeaderTypeServerPacket = 1

	MinPaddingLength = 0
	MaxPaddingLength = 900

	// type + 64-bit timestamp + socks address + padding length + padding
	TCPReqHeaderMaxLength = 1 + 8 + socks.MaxAddrLen + 2 + MaxPaddingLength

	// type + 64-bit timestamp + max salt length
	TCPRespHeaderMaxLength = 1 + 8 + 32

	SeparateHeaderMinClientPacketID = 0
	SeparateHeaderMinServerPacketID = 1 << 63
)

var (
	ErrBadTimestamp            = errors.New("time diff is over 30 seconds")
	ErrTypeMismatch            = errors.New("header type mismatch")
	ErrPaddingLengthOutOfRange = errors.New("padding length is less than 0 or greater than 900")
	ErrClientSaltMismatch      = errors.New("client salt in response header does not match request")
	ErrSessionIDMismatch       = errors.New("unexpected session ID")

	tcpReqHeaderPool = slicepool.MakePool(TCPReqHeaderMaxLength)
)

func ParseTCPReqHeader(r io.Reader, cipherConfig CipherConfig, htype byte) (string, error) {
	if !cipherConfig.IsSpec2022 {
		a, err := socks.AddrFromReader(r)
		if err != nil {
			return "", err
		}
		return a.String(), nil
	}

	lazySlice := tcpReqHeaderPool.LazySlice()
	b := lazySlice.Acquire()
	defer lazySlice.Release()

	// Read type & timestamp
	_, err := io.ReadFull(r, b[:1+8])
	if err != nil {
		return "", fmt.Errorf("failed to read type and timestamp: %w", err)
	}

	// Verify type
	if b[0] != htype {
		return "", ErrTypeMismatch
	}

	// Verify timestamp
	epoch := int64(binary.BigEndian.Uint64(b[1 : 1+8]))
	nowEpoch := time.Now().Unix()
	diff := epoch - nowEpoch
	if diff < -30 || diff > 30 {
		return "", ErrBadTimestamp
	}

	offset := 1 + 8

	// Read socks address
	n, err := socks.ReadAddr(b[offset:], r)
	if err != nil {
		return "", fmt.Errorf("failed to read socks address: %w", err)
	}
	socksaddr := socks.Addr(b[offset : offset+n])
	offset += n

	// Read padding length
	_, err = io.ReadFull(r, b[offset:offset+2])
	if err != nil {
		return "", fmt.Errorf("failed to read padding length: %w", err)
	}

	// Verify padding length
	paddingLen := int(binary.BigEndian.Uint16(b[offset : offset+2]))
	if paddingLen < MinPaddingLength || paddingLen > MaxPaddingLength {
		return "", ErrPaddingLengthOutOfRange
	}

	// Read padding
	if paddingLen > 0 {
		_, err := io.ReadFull(r, b[offset+2:offset+2+paddingLen])
		if err != nil {
			return "", fmt.Errorf("failed to read padding: %w", err)
		}
	}

	return socksaddr.String(), nil
}

func WriteTCPReqHeader(dst, socksaddr []byte, addPadding bool, cipherConfig CipherConfig) (n int) {
	if !cipherConfig.IsSpec2022 {
		copy(dst, socksaddr)
		return len(socksaddr)
	}

	// Write type
	dst[0] = HeaderTypeClientStream

	// Write timestamp
	nowEpoch := time.Now().Unix()
	binary.BigEndian.PutUint64(dst[1:], uint64(nowEpoch))

	n = 1 + 8

	// Write socks address
	n += copy(dst[n:], socksaddr)

	// Write padding if applicable
	// We pass 53 as port number so whether padding is added depends on maxPaddingLen.
	var maxPaddingLen int
	if addPadding {
		maxPaddingLen = MaxPaddingLength
	}
	n += WriteRandomPadding(dst[n:], 53, maxPaddingLen)

	return
}

func ParseTCPRespHeader(r io.Reader, clientSalt []byte, cipherConfig CipherConfig) error {
	if !cipherConfig.IsSpec2022 {
		return nil
	}

	b := make([]byte, 1+8+len(clientSalt))

	// Read response header
	_, err := io.ReadFull(r, b)
	if err != nil {
		return fmt.Errorf("failed to read response header: %w", err)
	}

	// Verify type
	if b[0] != HeaderTypeServerStream {
		return ErrTypeMismatch
	}

	// Verify timestamp
	epoch := int64(binary.BigEndian.Uint64(b[1 : 1+8]))
	nowEpoch := time.Now().Unix()
	diff := epoch - nowEpoch
	if diff < -30 || diff > 30 {
		return ErrBadTimestamp
	}

	// Verify client salt
	n := bytes.Compare(clientSalt, b[1+8:])
	if n != 0 {
		return ErrClientSaltMismatch
	}

	return nil
}

func WriteTCPRespHeader(dst, clientSalt []byte, cipherConfig CipherConfig) (n int) {
	if !cipherConfig.IsSpec2022 {
		return 0
	}

	dst[0] = HeaderTypeServerStream

	nowEpoch := time.Now().Unix()
	binary.BigEndian.PutUint64(dst[1:1+8], uint64(nowEpoch))

	n = 1 + 8
	n += copy(dst[1+8:], clientSalt)
	return
}

// For spec 2022, this function only parses the decrypted AEAD header.
func ParseUDPHeader(plaintext []byte, htype byte, cipherConfig CipherConfig) (socksAddrStart int, socksAddr socks.Addr, payload []byte, err error) {
	var offset int

	if cipherConfig.IsSpec2022 {
		// Filter out short packets
		if len(plaintext) < 16+1+8+3 {
			err = fmt.Errorf("packet too short: %d", len(plaintext))
			return
		}

		// Session ID, packet ID
		offset += 16

		// Verify type
		if plaintext[offset] != htype {
			err = ErrTypeMismatch
			return
		}

		offset++

		// Verify timestamp
		epoch := int64(binary.BigEndian.Uint64(plaintext[offset : offset+8]))
		nowEpoch := time.Now().Unix()
		diff := epoch - nowEpoch
		if diff < -30 || diff > 30 {
			err = ErrBadTimestamp
			return
		}

		offset += 8

		// Verify padding length
		if len(plaintext) < offset+2 {
			err = fmt.Errorf("packet too short to contain padding length field: %d", len(plaintext))
			return
		}

		paddingLen := int(binary.BigEndian.Uint16(plaintext[offset : offset+2]))
		if paddingLen < MinPaddingLength || paddingLen > MaxPaddingLength {
			err = ErrPaddingLengthOutOfRange
			return
		}

		offset += 2

		// Verify padding
		if len(plaintext) < offset+paddingLen {
			err = fmt.Errorf("packet too short (%d) to contain specified length (%d) of padding", len(plaintext), paddingLen)
			return
		}

		offset += paddingLen
	}

	socksAddrStart = offset

	// Parse socks address
	socksAddr, err = socks.SplitAddr(plaintext[offset:])
	if err != nil {
		err = fmt.Errorf("failed to parse target address: %w", err)
		return
	}

	offset += len(socksAddr)
	payload = plaintext[offset:]

	return
}

func WritePadding(b []byte, paddingLen int) int {
	if paddingLen == 0 {
		b[0] = 0
		b[1] = 0
		return 2
	}
	binary.BigEndian.PutUint16(b, uint16(paddingLen))
	return 2 + paddingLen
}

func WriteRandomPadding(b []byte, targetPort int, max int) int {
	if max == 0 || targetPort != 53 || len(b) < max {
		b[0] = 0
		b[1] = 0
		return 2
	}

	paddingLen := rand.Intn(max)
	binary.BigEndian.PutUint16(b, uint16(paddingLen))
	return 2 + paddingLen
}

// WriteUDPHeader fills a Shadowsocks 2022 UDP header into the buffer.
// Make sure to increment packet ID upon returning.
// Pass either targetUDPAddr or targetSocksAddr.
//
// For legacy Shadowsocks, call WriteUDPAddrToSocksAddr directly.
//
// No buffer length checks are performed.
// Make sure the buffer can hold the socks address.
func WriteUDPHeader(plaintext []byte, htype byte, sid []byte, pid uint64, targetUDPAddr *net.UDPAddr, targetSocksAddr []byte, paddingLen int) (n int) {
	// Write session ID
	n = copy(plaintext[:8], sid)

	// Write packet ID
	binary.BigEndian.PutUint64(plaintext[n:n+8], pid)
	n += 8

	// Write type
	plaintext[n] = htype
	n++

	// Write timestamp
	nowEpoch := time.Now().Unix()
	binary.BigEndian.PutUint64(plaintext[n:n+8], uint64(nowEpoch))
	n += 8

	// Write padding length and optionally padding
	n += WritePadding(plaintext[n:], paddingLen)

	// Write socks address
	switch {
	case targetUDPAddr != nil:
		n += socks.WriteUDPAddrAsSocksAddr(plaintext[n:], targetUDPAddr)
	case targetSocksAddr != nil:
		n += copy(plaintext[n:], targetSocksAddr)
	}

	return
}

// WriteClientUDPHeader fills a Shadowsocks UDP header into the buffer.
//
// For Shadowsocks 2022, make sure to increment packet ID upon returning.
//
// No buffer length checks are performed.
// Make sure the buffer can hold the socks address.
func WriteClientUDPHeader(plaintext []byte, cipherConfig CipherConfig, sid []byte, pid uint64, targetAddr net.Addr, maxPacketSize int) (n int, err error) {
	if cipherConfig.IsSpec2022 {
		n += WriteUDPHeader(plaintext[n:], HeaderTypeClientPacket, sid, pid, nil, nil, 0)
		// Go back so we can rewrite/overwrite padding length.
		n -= 2
	}

	// Determine port and write padding, socks address
	var port int

	if udpaddr, ok := targetAddr.(*net.UDPAddr); ok {
		port = udpaddr.Port

		// Write padding length and optionally padding
		if cipherConfig.IsSpec2022 {
			n += WriteRandomPadding(plaintext[n:], port, maxPacketSize-n-2)
		}

		n += socks.WriteUDPAddrAsSocksAddr(plaintext[n:], udpaddr)
	} else {
		host, portString, err := net.SplitHostPort(targetAddr.String())
		if err != nil {
			return n, fmt.Errorf("failed to split host:port: %w", err)
		}

		portnum, err := strconv.ParseUint(portString, 10, 16)
		if err != nil {
			return n, fmt.Errorf("failed to parse port string: %w", err)
		}
		port = int(portnum)

		// Write padding length and optionally padding
		if cipherConfig.IsSpec2022 {
			n += WriteRandomPadding(plaintext[n:], port, maxPacketSize-n-2)
		}

		if ip := net.ParseIP(host); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				plaintext[n] = socks.AtypIPv4
				n++
				n += copy(plaintext[n:], ip4)
			} else {
				plaintext[n] = socks.AtypIPv6
				n++
				n += copy(plaintext[n:], ip)
			}
		} else {
			if len(host) > 255 {
				return n, fmt.Errorf("host is too long: %d, must not be greater than 255", len(host))
			}
			plaintext[n] = socks.AtypDomainName
			n++
			plaintext[n] = byte(len(host))
			n++
			n += copy(plaintext[n:], host)
		}

		binary.BigEndian.PutUint16(plaintext[n:], uint16(port))
		n += 2
	}

	return
}
