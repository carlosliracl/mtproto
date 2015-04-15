package mtproto

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"reflect"
)

type MTProto struct {
	conn *net.TCPConn

	// данные соединения
	g_ab        *big.Int
	authKey     []byte
	authKeyHash []byte
	serverSalt  []byte
	encrypted   bool

	// буфер для пакета и данные его парсинга
	buf       []byte
	size      int
	off       int
	level     int
	messageId uint64
	seqNo     int32

	// разобранная структура
	data interface{}
}

func (m *MTProto) Connect(addr string) error {
	var err error
	var tcpAddr *net.TCPAddr

	m.g_ab = big.NewInt(0)
	m.encrypted = false

	tcpAddr, err = net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return err
	}

	m.conn, err = net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		return err
	}

	_, err = m.conn.Write([]byte{0xef})
	if err != nil {
		return err
	}

	return nil
}

func (m *MTProto) SendPacket(msg []byte) error {
	x := make([]byte, 0, 256)

	if m.encrypted {
		// TODO: encrypt packet

	} else {
		x = append(x, EncodeLong(0)...)
		x = append(x, EncodeLong(GenerateMessageId())...)
		x = append(x, EncodeInt(int32(len(msg)))...)
		x = append(x, msg...)

	}

	size := len(x) / 4
	if size < 127 {
		x = append([]byte{byte(size)}, x...)
	} else {
		x = append(EncodeInt(int32(size<<8|127)), x...)
	}

	_, err := m.conn.Write(x)
	if err != nil {
		return err
	}

	return nil
}

func (m *MTProto) Handshake() error {
	var x []byte
	var err error

	// (send) req_pq
	nonceFirst := GenerateNonce(16)
	err = m.SendPacket(Encode_TL_req_pq(nonceFirst))
	if err != nil {
		return err
	}

	// (parse) resPQ
	err = m.Read()
	if err != nil {
		return err
	}
	res, ok := m.data.(TL_resPQ)
	if !ok {
		return errors.New("Handshake: ожидался resPQ")
	}
	if !bytes.Equal(nonceFirst, res.nonce) {
		return errors.New("Handshake: не совпадает nonce")
	}
	found := false
	for _, b := range res.fingerprints {
		if b == telegramPublicKey_FP {
			found = true
			break
		}
	}
	if !found {
		return errors.New("Handshake: нет отпечатка нужного ключа")
	}

	// (encoding) p_q_inner_data
	p, q := SplitPQ(res.pq)
	nonceSecond := GenerateNonce(32)
	nonceServer := res.server_nonce
	innerData1 := Encode_TL_p_q_inner_data(res.pq, p, q, nonceFirst, nonceServer, nonceSecond)

	x = make([]byte, 255)
	copy(x[0:], Sha1(innerData1))
	copy(x[20:], innerData1)
	encryptedData1 := RSAEncode(x)

	// (send) req_DH_params
	err = m.SendPacket(Encode_TL_req_DH_params(nonceFirst, nonceServer, p, q, telegramPublicKey_FP, encryptedData1))
	if err != nil {
		return err
	}

	// (parse) server_DH_params_{ok, fail}
	err = m.Read()
	if err != nil {
		return err
	}
	dh, ok := m.data.(TL_server_DH_params_ok)
	if !ok {
		return errors.New("Handshake: ожидался server_DH_params_ok")
	}
	if !bytes.Equal(nonceFirst, dh.nonce) {
		return errors.New("Handshake: не совпадает nonce")
	}
	if !bytes.Equal(nonceServer, dh.server_nonce) {
		return errors.New("Handshake: не совпадает server_nonce")
	}
	t1 := make([]byte, 48)
	copy(t1[0:], nonceSecond)
	copy(t1[32:], nonceServer)
	hash1 := Sha1(t1)

	t2 := make([]byte, 48)
	copy(t2[0:], nonceServer)
	copy(t2[16:], nonceSecond)
	hash2 := Sha1(t2)

	t3 := make([]byte, 64)
	copy(t3[0:], nonceSecond)
	copy(t3[32:], nonceSecond)
	hash3 := Sha1(t3)

	tmpAESKey := make([]byte, 32)
	tmpAESIV := make([]byte, 32)

	copy(tmpAESKey[0:], hash1)
	copy(tmpAESKey[20:], hash2[0:12])

	copy(tmpAESIV[0:], hash2[12:20])
	copy(tmpAESIV[8:], hash3)
	copy(tmpAESIV[28:], nonceSecond[0:4])

	// (parse-thru) server_DH_inner_data
	decodedData, err := AES256IGE_decrypt(dh.encrypted_answer, tmpAESKey, tmpAESIV)
	if err != nil {
		return err
	}
	m.buf = decodedData[20:]
	m.size = len(m.buf)
	m.off = 0
	m.level = 0
	err = m.DecodePacket()
	if err != nil {
		return err
	}
	dhi, ok := m.data.(TL_server_DH_inner_data)
	if !ok {
		return errors.New("Handshake: ожидался server_DH_inner_data")
	}
	if !bytes.Equal(nonceFirst, dhi.nonce) {
		return errors.New("Handshake: не совпадает nonce")
	}
	if !bytes.Equal(nonceServer, dhi.server_nonce) {
		return errors.New("Handshake: не совпадает server_nonce")
	}

	_, g_b, g_ab := MakeGAB(dhi.g, dhi.g_a, dhi.dh_prime)
	authKey := g_ab.Bytes()
	if authKey[0] == 0 {
		authKey = authKey[1:]
	}
	t4 := make([]byte, 32+1+8)
	copy(t4[0:], nonceSecond)
	t4[32] = 1
	copy(t4[33:], Sha1(authKey)[0:8])
	nonceHash1 := Sha1(t4)[4:20]
	serverSalt := make([]byte, 8)
	copy(serverSalt, nonceSecond[:8])
	Xor(serverSalt, nonceServer[:8])

	// (encoding) client_DH_inner_data
	innerData2 := Encode_TL_client_DH_inner_data(nonceFirst, nonceServer, 0, g_b)
	x = make([]byte, 20+len(innerData2)+(16-((20+len(innerData2))%16))&15)
	copy(x[0:], Sha1(innerData2))
	copy(x[20:], innerData2)
	encryptedData2, err := AES256IGE_encrypt(x, tmpAESKey, tmpAESIV)

	// (send) set_client_DH_params
	err = m.SendPacket(Encode_TL_set_client_DH_params(nonceFirst, nonceServer, encryptedData2))
	if err != nil {
		return err
	}

	// (parse) dh_gen_{ok, retry, fail}
	err = m.Read()
	if err != nil {
		return err
	}
	dhg, ok := m.data.(TL_dh_gen_ok)
	if !ok {
		return errors.New("Handshake: ожидался dh_gen_ok")
	}
	if !bytes.Equal(nonceFirst, dhg.nonce) {
		return errors.New("Handshake: не совпадает nonce")
	}
	if !bytes.Equal(nonceServer, dhg.server_nonce) {
		return errors.New("Handshake: не совпадает server_nonce")
	}
	if !bytes.Equal(nonceHash1, dhg.new_nonce_hash1) {
		return errors.New("Handshake: не совпадает new_nonce_hash1")
	}

	// (all ok)
	m.setGAB(g_ab)
	m.setSalt(serverSalt)

	return nil
}

func (m *MTProto) Read() error {
	var err error
	var n int

	b := make([]byte, 1)
	n, err = m.conn.Read(b)
	if err != nil {
		return err
	}

	if b[0] < 127 {
		m.size = int(b[0]) << 2
	} else {
		b := make([]byte, 3)
		n, err = m.conn.Read(b)
		if err != nil {
			return err
		}
		m.size = (int(b[0]) | int(b[1])<<8 | int(b[2])<<16) << 2
	}

	left := m.size
	m.buf = make([]byte, m.size)
	for left > 0 {
		n, err = m.conn.Read(m.buf[m.size-left:])
		if err != nil {
			return err
		}
		left -= n
	}
	m.off = 0

	if m.size == 4 {
		return fmt.Errorf("Ошибка сервера: %d", int32(binary.LittleEndian.Uint32(m.buf)))
	}

	if m.size <= 8 {
		return fmt.Errorf("Слишком маленький пакет: %d байт", m.size)
	}

	authKeyHash, err := m.DecodeLong()
	if authKeyHash == 0 {
		m.messageId, err = m.DecodeLong()
		if err != nil {
			return err
		}
		messageLen, err := m.DecodeInt()
		if err != nil {
			return err
		}
		if int(messageLen) != m.size-20 {
			return fmt.Errorf("Длина сообщения не совпадает: %d (должна быть %d)", messageLen, m.size-20)
		}
		mod := m.messageId & 3
		if mod != 1 && mod != 3 {
			return fmt.Errorf("Невалидные битые message_id: %d", mod)
		}
		m.seqNo = 0
		m.level = 0

		err = m.DecodePacket()
		if err != nil {
			return err
		}

	} else {
		// TODO: read encrypted packet

	}

	return nil
}

func (m *MTProto) setGAB(g_ab *big.Int) {
	m.g_ab = g_ab
	m.authKey = g_ab.Bytes()
	if m.authKey[0] == 0 {
		m.authKey = m.authKey[1:]
	}
	m.authKeyHash = Sha1(m.authKey)[12:20]
	m.encrypted = g_ab.Cmp(big.NewInt(0)) != 0
}

func (m *MTProto) setSalt(s []byte) {
	m.serverSalt = s
}

func (m *MTProto) Dump() {
	fmt.Printf("AB: %v\tSALT: %v\tConnection: %v\n", m.g_ab, m.serverSalt, m.conn)
	fmt.Println(reflect.TypeOf(m.data), m.data)
}

func Dump(x []byte) {
	fmt.Println(hex.Dump(x))
}