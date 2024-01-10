package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/mediadevices"
	"github.com/pion/mediadevices/pkg/codec/vpx"
	_ "github.com/pion/mediadevices/pkg/driver/camera"
	_ "github.com/pion/mediadevices/pkg/driver/microphone"
	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/pion/webrtc/v3"
	"github.com/sourcegraph/jsonrpc2"
	"log"
	"net/url"
)

var addr string
var peerConnection *webrtc.PeerConnection
var connectionID uint64
var remoteDescription *webrtc.SessionDescription

type Candidate struct {
	Target    int                  `json:"target"`
	Candidate *webrtc.ICECandidate `json:candidate`
}

type SendOffer struct {
	SID   string                     `json:sid`
	Offer *webrtc.SessionDescription `json:offer`
}

type Response struct {
	Params *webrtc.SessionDescription `json:params`
	Result *webrtc.SessionDescription `json:result`
	Method string                     `json:method`
	Id     uint64                     `json:id`
}
type ResponseCandidate struct {
	Target    int                      `json:"target"`
	Candidate *webrtc.ICECandidateInit `json:candidate`
}

type SendAnswer struct {
	SID    string                     `json:sid`
	Answer *webrtc.SessionDescription `json:answer`
}

// TrickleResponse received from the sfu server
type TrickleResponse struct {
	Params ResponseCandidate `json:params`
	Method string            `json:method`
}

func main() {
	flag.StringVar(&addr, "a", "localhost:7001", "address to use")
	flag.Parse()

	u := url.URL{Scheme: "ws", Host: addr, Path: "/ws"}
	log.Printf("connecting to: %s", u.String())
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("dial: %s", err)
	}
	defer func(c *websocket.Conn) {
		err := c.Close()
		if err != nil {
			log.Fatalf("error while close ws connection: %s", err)
		}
	}(c)

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	}

	mediaEngine := webrtc.MediaEngine{}

	vpxParams, err := vpx.NewVP8Params()
	if err != nil {
		panic(err)
	}
	vpxParams.BitRate = 500_000 //500 kbps

	codecSelector := mediadevices.NewCodecSelector(
		mediadevices.WithVideoEncoders(&vpxParams),
	)
	codecSelector.Populate(&mediaEngine)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&mediaEngine))
	peerConnection, err = api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	done := make(chan struct{})
	go readMessage(c, done)
	fmt.Println(mediadevices.EnumerateDevices())
	s, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Video: func(constraints *mediadevices.MediaTrackConstraints) {
			constraints.FrameFormat = prop.FrameFormat(frame.FormatYUY2)
			constraints.Width = prop.Int(640)
			constraints.Height = prop.Int(480)
		},
		Codec: codecSelector,
	})

	if err != nil {
		panic(err)
	}
	for _, track := range s.GetTracks() {
		track.OnEnded(func(err error) {
			fmt.Printf("Track with ID: %s ended with error: %v", track.ID(), err)
		})
		_, err := peerConnection.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
		})
		if err != nil {
			panic(err)
		}
	}

	offer, err := peerConnection.CreateOffer(nil)
	err = peerConnection.SetLocalDescription(offer)
	if err != nil {
		panic(err)
	}

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			candidateJson, err := json.Marshal(&Candidate{
				Candidate: candidate,
				Target:    0,
			})
			params := (*json.RawMessage)(&candidateJson)
			if err != nil {
				log.Fatal(err)
			}
			message := jsonrpc2.Request{
				Method: "trickle",
				Params: params,
			}

			reqBodyBytes := new(bytes.Buffer)
			err = json.NewEncoder(reqBodyBytes).Encode(message)
			if err != nil {
				log.Fatal(err)
			}
			messageBytes := reqBodyBytes.Bytes()
			err = c.WriteMessage(websocket.TextMessage, messageBytes)
			if err != nil {
				log.Fatal(err)
			}
		}
	})

	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed to %s \n", state.String())
	})

	offerJson, err := json.Marshal(&SendOffer{
		Offer: peerConnection.LocalDescription(),
		SID:   "1819cd84-f0be-498f-bc49-19f1f7d2f585",
	})

	params := (*json.RawMessage)(&offerJson)

	connectionUUID := uuid.New()
	connectionID = uint64(connectionUUID.ID())

	offerMessage := &jsonrpc2.Request{
		Method: "join",
		Params: params,
		ID: jsonrpc2.ID{
			IsString: false,
			Num:      connectionID,
			Str:      "",
		},
	}

	reqBodyBytes := new(bytes.Buffer)
	err = json.NewEncoder(reqBodyBytes).Encode(offerMessage)
	if err != nil {
		log.Fatal(err)
	}
	messageBytes := reqBodyBytes.Bytes()
	err = c.WriteMessage(websocket.TextMessage, messageBytes)
	if err != nil {
		log.Fatal(err)
	}

	<-done
}

func readMessage(c *websocket.Conn, done chan struct{}) {
	defer close(done)
	for {
		_, message, err := c.ReadMessage()
		if err != nil {
			log.Fatalf("error while reading: %s", err)
		}
		log.Printf("recv: %s", message)
		var response Response
		err = json.Unmarshal(message, &response)
		if err != nil {
			log.Printf("Error while unmarshal message: %s", message)
		}

		if response.Id == connectionID {
			result := *response.Result
			fmt.Printf("result: %s", result)
			if err := peerConnection.SetRemoteDescription(result); err != nil {
				log.Fatal(err)
			}
		} else if response.Id != 0 && response.Method == "offer" {
			err := peerConnection.SetRemoteDescription(*response.Params)
			if err != nil {
				log.Fatal(err)
			}
			answer, err := peerConnection.CreateAnswer(nil)
			if err != nil {
				log.Fatal(err)
			}
			err = peerConnection.SetLocalDescription(answer)
			if err != nil {
				log.Fatal(err)
			}
			connectionUUID := uuid.New()
			connectionID = uint64(connectionUUID.ID())
			offerJson, err := json.Marshal(&SendAnswer{
				Answer: remoteDescription,
				SID:    "1819cd84-f0be-498f-bc49-19f1f7d2f585",
			})
			params := (*json.RawMessage)(&offerJson)
			answerMessage := &jsonrpc2.Request{
				Method: "answer",
				Params: params,
				ID: jsonrpc2.ID{
					IsString: false,
					Num:      connectionID,
					Str:      "",
				},
			}

			reqBodyBytes := new(bytes.Buffer)
			err = json.NewEncoder(reqBodyBytes).Encode(answerMessage)
			if err != nil {
				log.Fatal(err)
			}
			messageBytes := reqBodyBytes.Bytes()
			err = c.WriteMessage(websocket.TextMessage, messageBytes)
			if err != nil {
				log.Fatal(err)
			}
		} else if response.Method == "trickle" {
			var trickleResponse TrickleResponse
			if err := json.Unmarshal(message, &trickleResponse); err != nil {
				log.Fatal(err)
			}
			err := peerConnection.AddICECandidate(*trickleResponse.Params.Candidate)
			if err != nil {
				log.Fatal(err)
			}
		}
	}
}
