package libhttpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func ParsePacket(data []byte) UDPPacket {
	pType := data[0]
	seqNo := data[1:5]
	peerAddr := data[5:9]
	peerPort := data[9:11]
	payload := data[11:]

	return UDPPacket{
		pType:    []byte{pType},
		seqNo:    seqNo,
		peerAddr: peerAddr,
		peerPort: peerPort,
		payload:  payload,
	}
}

func makePacket(pType uint32, seqNo uint32, parsedURL *url.URL, payload string) UDPPacket {

	// pType, one of the following: 0 - Data, 1- ACK, 2 - SYN, 3 - SYN-ACK, 4 - NAK; 1 byte
	pTypeByte := []byte{byte(pType)}

	// seqNo, for SYN it is the initial pNo during 3WH -- else incremental packet numbers; 4 bytes BE
	seqNoBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(seqNoBytes, seqNo)

	// peerAddr, either sender/receiver -- translated by router!; 4 bytes
	peerAddrBytes := make([]byte, 4)
	addrSplit := strings.Split(parsedURL.Host, ":")
	peerAddr := addrSplit[0]
	peerAddrSplit := strings.Split(peerAddr, ".")
	for i, section := range peerAddrSplit {
		intSection, _ := strconv.Atoi(section)
		peerAddrBytes[i] = byte(intSection)
	}

	//peerAddrBytes := make([]byte, 4)
	//binary.BigEndian.PutUint32(peerAddrBytes, peerAddr)

	// peerPort, either sender/receiver -- translated by router!; 2 bytes BE
	peerPortBytes := make([]byte, 2)
	peerPortInt, _ := strconv.Atoi(addrSplit[1])
	binary.BigEndian.PutUint16(peerPortBytes, uint16(peerPortInt))

	// payload; max 1013 bytes
	// TODO handle size constraints/breaking somehow...
	payloadBytes := []byte(payload)

	// Packet Size Range: 11 (no payload) to 1024 (full payload)

	return UDPPacket{
		pType:    pTypeByte,
		seqNo:    seqNoBytes,
		peerAddr: peerAddrBytes,
		peerPort: peerPortBytes,
		payload:  payloadBytes,
	}
}

func getDataPacketBytes(seqNo uint32, parsedURL *url.URL, payload string) ([][]byte, int) {
	numPackets := int(math.Ceil(float64(len(payload)) / float64(1013)))
	packetsBytes := make([][]byte, numPackets)
	payloadBytes := []byte(payload)

	if numPackets == 1 {
		packetBytes := getBytesFromPacket(makePacket(0, seqNo, parsedURL, payload))
		packetsBytes[0] = packetBytes
		return packetsBytes, 1
	}

	counter := 0
	for i := 1; i < numPackets; i++ {
		chunk := payloadBytes[counter : counter+1013]
		packetForChunk := makePacket(0, seqNo, parsedURL, string(chunk))
		packetsBytes[i-1] = getBytesFromPacket(packetForChunk)
		counter += 1013
		seqNo++
	}
	residue := len(payload) % 1013
	if residue > 0 {
		residueChunk := payloadBytes[counter:]
		packetsBytes[numPackets-1] = getBytesFromPacket(makePacket(0, seqNo, parsedURL, string(residueChunk)))
	}
	return packetsBytes, numPackets
}

func handshake(conn *net.UDPConn, parsedURL *url.URL, numPackets int) {
	for {
		deadline := time.Now().Add(2 * time.Second)
		//wTimeoutErr := conn.SetWriteDeadline(deadline)
		rTimeoutErr := conn.SetReadDeadline(deadline)
		//if wTimeoutErr != nil || rTimeoutErr != nil {
		if rTimeoutErr != nil {
			fmt.Println("Timing out!")
		}

		seqInit := uint32(1)
		packet := makePacket(2, seqInit, parsedURL, fmt.Sprintf("%d", numPackets))
		packetBytes := getBytesFromPacket(packet)

		_, err := conn.Write(packetBytes)
		if err != nil {
			fmt.Println(err)
		}

		readBuf := make([]byte, 11)
		_, _, readErr := conn.ReadFromUDP(readBuf)
		if readErr != nil {
			fmt.Println("I/O timeout, retransmissing...")
			continue
		}

		synAck := ParsePacket(readBuf)
		receivedSeq := binary.BigEndian.Uint32(synAck.seqNo)
		if synAck.pType[0] == 3 && receivedSeq == seqInit+1 {
			packet = makePacket(1, receivedSeq+1, parsedURL, "")
			packetBytes = getBytesFromPacket(packet)

			_, err := conn.Write(packetBytes)
			if err != nil {
				fmt.Println(err)
			}
			break
		} else {
			fmt.Println("Invalid packet type or sequence number, ignoring.")
		}
	}
}

func getBytesFromPacket(packet UDPPacket) []byte {
	packetBytes := append(packet.pType, packet.seqNo...)
	packetBytes = append(packetBytes, packet.peerAddr...)
	packetBytes = append(packetBytes, packet.peerPort...)
	packetBytes = append(packetBytes, packet.payload...)
	return packetBytes
}

func remove(packetList []UDPPacket, removePack UDPPacket) []UDPPacket {
	for i, curr := range packetList {
		if binary.BigEndian.Uint32(curr.seqNo) == binary.BigEndian.Uint32(removePack.seqNo) {
			return append(packetList[:i], packetList[i+1:]...)
		}
	}
	return packetList
}

func UDPGet(inputUrl string, headers RequestHeader) (string, error) {
	parsedURL, parsedHeaders, conn, err := udpConnectHandler(inputUrl, headers)

	if err != nil {
		return BlankString, err
	}

	defer conn.Close()
	requestString := fmt.Sprintf(
		"GET %s %s%s%s%s%s",
		parsedURL.RequestURI(), ProtocolVersion, CRLF,
		parsedHeaders, CRLF, CRLF)

	packets, numPackets := getDataPacketBytes(4, parsedURL, requestString)

	// make handshake
	handshake(conn, parsedURL, numPackets)

	// start a goroutine listener for the ACKs/NAKs
	packetChan := make(chan UDPPacket)

	var responsePayload []string
	numOfResponsePackets := -1
	var nakList []UDPPacket

	go func() {
		// if NAKs are dropped, try again...
		for packet := range packetChan {
			if packet.pType[0] == 4 {
				fmt.Println("Handling NAK")
				missingNo := binary.BigEndian.Uint32(packet.seqNo)
				missingPacket := packets[int(missingNo)-4]
				_, err = conn.Write(missingPacket)
				if err != nil {
					fmt.Println(err)
				}
			}
		}
	}()

	for _, packetBytes := range packets {
		_, err = conn.Write(packetBytes)
		if err != nil {
			fmt.Println(err)
		}
	}

	var responsePacket UDPPacket
	var expectedSeqNo uint32
	expectedSeqNo = 1
	acks := make([]uint32, 5)
	naks := make([]uint32, 5)

	for {
		readBuf := make([]byte, 1024)
		_, _, readErr := conn.ReadFromUDP(readBuf)
		responsePacket = ParsePacket(readBuf)
		if readErr != nil {
			continue
		}

		if responsePacket.pType[0] == 1 || responsePacket.pType[0] == 4 {
			select {
			case packetChan <- responsePacket:
			default:
				// loop again
			}
		}

		if responsePacket.pType[0] == 4 {
			nakList = append(nakList, responsePacket)
		} else if responsePacket.pType[0] == 1 {
			nakList = remove(nakList, responsePacket)
		}

		if responsePacket.pType[0] == 0 {
			if numOfResponsePackets == -1 {
				numOfResponsePackets = int(responsePacket.payload[len(responsePacket.payload)-1])
				if numOfResponsePackets == 0 {
					numOfResponsePackets = 1
				}
				responsePayload = make([]string, numOfResponsePackets+5)
			}
			responseSeq := binary.BigEndian.Uint32(responsePacket.seqNo)

			responsePacketPayloadLength := len(responsePacket.payload) - 1
			responseSlice := responsePacket.payload[0 : responsePacketPayloadLength-1]
			responsePayload[responseSeq] = string(responseSlice)
			acks = append(acks, responseSeq)
			if responseSeq == expectedSeqNo {
				ackPacket := makePacket(4, responseSeq, parsedURL, "")
				packetBytes := getBytesFromPacket(ackPacket)
				_, writeErr := conn.Write(packetBytes)
				if writeErr != nil {
					fmt.Println("Timeout writing ACK!")
				}
				//fmt.Println(fmt.Sprintf("ACK'd packet %d", responseSeq))
				expectedSeqNo++
			} else if responseSeq < expectedSeqNo {
				// SEND ACK
				ackPacket := makePacket(4, responseSeq, parsedURL, "")
				packetBytes := getBytesFromPacket(ackPacket)
				_, writeErr := conn.Write(packetBytes)
				if writeErr != nil {
					fmt.Println("Timeout writing ACK!")
				}
				//fmt.Println(fmt.Sprintf("ACK'd packet %d", responseSeq))
			} else {
				for packetNum := expectedSeqNo; packetNum < responseSeq; packetNum++ {
					naks = append(naks, packetNum)
					nakPacket := makePacket(4, packetNum, parsedURL, "")
					packetBytes := getBytesFromPacket(nakPacket)
					_, writeErr := conn.Write(packetBytes)
					if writeErr != nil {
						fmt.Println("Timeout writing NAKs!")
					}
					//fmt.Println(fmt.Sprintf("NAK'd packet %d", packetNum))
				}
				expectedSeqNo = responseSeq + 1
			}
			if numOfResponsePackets == 1 {
				return responsePayload[responseSeq], nil
			} else if checkNotEmpty(responsePayload[1 : numOfResponsePackets+1]) {
				return stringifiedResponse(responsePayload[1 : numOfResponsePackets+1]), nil
			}

		}
	}
}

func stringifiedResponse(responsePayload []string) string {
	response := ""
	for _, stringifiedPacket := range responsePayload {
		response += stringifiedPacket
	}
	return response
}

func UDPPost(inputUrl string, headers RequestHeader, body []byte) (string, error) {
	headers["Content-Length"] = fmt.Sprintf("%d", len(body))
	parsedURL, parsedHeaders, conn, err := udpConnectHandler(inputUrl, headers)

	if err != nil {
		return BlankString, err
	}

	defer conn.Close()

	requestString := fmt.Sprintf("POST %s %s%s%s%s%s%s",
		parsedURL.RequestURI(), ProtocolVersion, CRLF,
		parsedHeaders, CRLF, body, CRLF)

	packets, numPackets := getDataPacketBytes(4, parsedURL, requestString)

	// ack packets
	ackPackets := make([]UDPPacket, numPackets)
	for i, copyPacket := range packets {
		ackPackets[i] = ParsePacket(copyPacket)
	}

	// make handshake
	handshake(conn, parsedURL, numPackets)

	// start a goroutine listener for the ACKs/NAKs
	packetChan := make(chan UDPPacket, 1024)

	go func() {
		var nakList []UDPPacket

		for packet := range packetChan {
			if packet.pType[0] == 4 {
				nakList = append(nakList, packet)
			} else if packet.pType[0] == 1 {
				nakList = remove(nakList, packet)
			}

			for _, nakPack := range nakList {
				fmt.Println("Sending NAK!")
				missingNo := binary.BigEndian.Uint32(nakPack.seqNo)
				missingPacket := packets[int(missingNo)-4]
				_, err = conn.Write(missingPacket)
				if err != nil {
					fmt.Println(err)
				}
			}
		}
	}()

	for _, packetBytes := range packets {
		_, err = conn.Write(packetBytes)
		if err != nil {
			fmt.Println(err)
		}
	}
	var responsePacket UDPPacket

	for {
		readBuf := make([]byte, 1024)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err = conn.ReadFromUDP(readBuf)
		responsePacket = ParsePacket(readBuf)

		if err != nil {
			// retransmission of packets not ACK'd
			for _, lostPacket := range ackPackets {
				fmt.Println("Sending lost packet!")
				_, err = conn.Write(getBytesFromPacket(lostPacket))
				if err != nil {
					fmt.Println(err)
				}
			}
			continue
		}

		if responsePacket.pType[0] == 1 || responsePacket.pType[0] == 4 {
			if responsePacket.pType[0] == 1 {
				ackPackets = remove(ackPackets, responsePacket)
			}
			select {
			case packetChan <- responsePacket:
				// do nothing
				packetChan <- responsePacket
			default:
				// buffer is full
				//fmt.Println("channel buffer overflow!")
			}
		}

		if responsePacket.pType[0] == 0 {
			break
		}
	}

	return string(responsePacket.payload), nil
}

func Get(inputUrl string, headers RequestHeader) (string, error) {
	parsedURL, parsedHeaders, conn, err := connectHandler(inputUrl, headers)

	if err != nil {
		return BlankString, err
	}

	defer conn.Close()
	requestString := fmt.Sprintf(
		"GET %s %s%s%s%s%s",
		parsedURL.RequestURI(), ProtocolVersion, CRLF,
		parsedHeaders, CRLF, CRLF)

	fmt.Fprintf(conn, requestString)
	response, err := readResponseFromConnection(conn)

	if err != nil {
		return BlankString, nil
	}

	return string(response), nil
}

func Post(inputUrl string, headers RequestHeader, body []byte) (string, error) {
	headers["Content-Length"] = fmt.Sprintf("%d", len(body))
	parsedURL, parsedHeaders, conn, err := connectHandler(inputUrl, headers)

	if err != nil {
		return BlankString, err
	}

	defer conn.Close()

	requestString := fmt.Sprintf("POST %s %s%s%s%s%s%s",
		parsedURL.RequestURI(), ProtocolVersion, CRLF,
		parsedHeaders, CRLF, body, CRLF)
	fmt.Fprintf(conn, requestString)

	fmt.Println(requestString)

	response, err := readResponseFromConnection(conn)

	if err != nil {
		return BlankString, err
	}

	return string(response), nil
}

func FromString(response string) (*Response, error) {
	responseSplit := strings.Split(response, CRLF+CRLF)
	// splits between (statusLine + headers) and Body
	if len(responseSplit) == 2 {
		response := Response{}
		preBody := responseSplit[0]
		body := responseSplit[1]

		preBodySplit := strings.Split(preBody, "\n")
		if strings.HasPrefix(preBodySplit[0], "HTTP") {
			statusLineSplit := strings.Split(preBodySplit[0], " ")
			response.Protocol = statusLineSplit[0]

			statusCode, err := parseStatusCode(statusLineSplit[1])

			if err != nil {
				return nil, err
			}

			response.StatusCode = statusCode
		}

		response.Headers = strings.Join(preBodySplit[1:], "\n")

		response.Body = body

		return &response, nil
	}
	return nil, nil
}

func HandleRedirects(response *Response, responseString string, headers RequestHeader, redirectCount int) (string, error) {
	var err error
	for ; redirectCount < 5; redirectCount++ {
		if response.StatusCode >= 301 && response.StatusCode <= 303 {
			redirectURI := extractRedirectURI(response.Headers)
			fmt.Printf("Encountered status code %d...Redirecting to %s\n", response.StatusCode, redirectURI)
			if redirectURI != "" {
				responseString, err = Get(redirectURI, headers)
				if err != nil {
					return "", err
				}

				response, err = FromString(responseString)
				if err != nil {
					return "", err
				}
			} else {
				return "", errors.New("Bad redirect URI in Location header")
			}
		} else {
			return responseString, nil
		}
	}
	return "", errors.New("Exceeded 5 redirects!")
}

func extractRedirectURI(headers string) string {
	headerLines := strings.Split(headers, "\n")
	for _, header := range headerLines {
		indexOfSeparator := strings.Index(header, ":")
		if indexOfSeparator > -1 {
			if header[:indexOfSeparator] == "Location" {
				uri := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(header[indexOfSeparator+1:], "\r"), "\n"))
				return uri
			}
		} else {
			break
		}
	}
	return ""
}

func parseStatusCode(statusCode string) (int, error) {
	code, err := strconv.Atoi(statusCode)
	if err != nil {
		return -1, err
	}
	return code, nil
}

func readResponseFromConnection(conn net.Conn) ([]byte, error) {
	temp := make([]byte, 1024)
	data := make([]byte, 0)
	length := 0

	for {
		n, err := conn.Read(temp)
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}

		data = append(data, temp[:n]...)
		length += n
	}

	return data, nil
}

func udpConnectHandler(inputUrl string, headers RequestHeader) (*url.URL, string, *net.UDPConn, error) {
	parsedURL, urlErr := url.Parse(inputUrl)
	parsedHeaders := stringifyHeaders(headers)

	if urlErr != nil {
		fmt.Println(urlErr)
	}

	host := fmt.Sprintf("%s:%s", RouterAddr, RouterPort)
	hostUdpAddr, err := net.ResolveUDPAddr("udp", host)
	if err != nil {
		fmt.Println(err)
	}
	conn, err := net.DialUDP("udp", nil, hostUdpAddr)

	return parsedURL, parsedHeaders, conn, err
}

func connectHandler(inputUrl string, headers RequestHeader) (*url.URL, string, net.Conn, error) {
	parsedURL, urlErr := url.Parse(inputUrl)
	parsedHeaders := stringifyHeaders(headers)

	if urlErr != nil {
		return nil, BlankString, nil, urlErr
	}

	port := parsedURL.Port()
	if port == BlankString {
		port = "80"
	}

	host := fmt.Sprintf("%s:%s", parsedURL.Hostname(), port)

	conn, err := net.Dial("tcp", host)
	return parsedURL, parsedHeaders, conn, err
}

func stringifyHeaders(headers RequestHeader) string {
	headersString := BlankString
	for headerKey, headerValue := range headers {
		headersString += fmt.Sprintf("%s:%s%s", headerKey, headerValue, CRLF)
	}
	return headersString
}

func checkNotEmpty(responsePayload []string) bool {
	for _, packet := range responsePayload {
		if len(packet) == 0 {
			return false
		}
	}
	return true
}
