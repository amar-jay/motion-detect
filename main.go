package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/ivfwriter"
	ffmpeg "github.com/u2takey/ffmpeg-go"
	"gocv.io/x/gocv"
)

const (
	infile  = "rtsp://wowzaec2demo.streamlock.net/vod/mp4:BigBuckBunny_115k.mov"
	frameX  = 860
	frameY  = 720
	minArea = 3000
)

type CVStatus string

var (
	cancel            = make(chan struct{})
	Stale    CVStatus = "Stale"
	Detected CVStatus = "Motion Detected"
)

func init() {
	runtime.LockOSThread()
}

func main() {
	r, w := io.Pipe()
	go func() {
		<-cancel
		log.Println("[exit] cleaning up")
		w.Close()
		r.Close()
		os.Exit(0)
	}()

	err := webrtc_conn(w)
	if err != nil {
		log.Println("[webrtc_conn] error:", err)
	}

	for {
		select {
		case <-video_capture(w, r):
			log.Println("[video_capture] error:", err)
		case <-motion_detect(r):
			log.Println("[motion_detect] error:", err)
		}
	}

}

// wait until user enters something
func stdin[T any](text string, obj T) error {
	r := bufio.NewReader(os.Stdin)
	fmt.Print(text + " -> ")

	var in string
	for {
		var err error
		in, err = r.ReadString('\n')
		if err != io.EOF {
			if err != nil {
				return err
			}
		}
		in = strings.TrimSpace(in)
		if len(in) > 0 {
			break
		}
	}

	b := []byte(in)
	if len(b) < 0 || b[0] != '{' {
		return fmt.Errorf("invalid input")
	}

	err := json.Unmarshal(b, obj)
	if err != nil {
		return err
	}

	fmt.Println("")

	return nil
}
func video_capture(w io.Writer, r io.Reader) <-chan error {

	// ffmpeg (860, 720) file rawvideo pip
	log.Println("Starting ffmpeg process")
	done := make(chan error)
	go func() {
		err := ffmpeg.Input(
			"pipe:",
			ffmpeg.KwArgs{
				"format": "rawvideo", "pix_fmt": "rgb24",
				"s": fmt.Sprintf("%dx%d", frameX, frameY),
			}).
			Output("pipe:",
				ffmpeg.KwArgs{
					"format": "rawvideo", "pix_fmt": "rgb24",
				}).
			WithOutput(w).
			WithInput(r).
			Run()
		log.Println("ffmpeg process1 done")
		done <- err
		close(done)
		if err != nil {
			cancel <- struct{}{}
		}
	}()
	return done
}

func motion_detect(out io.Reader) <-chan error {
	done := make(chan error)

	window := gocv.NewWindow("Motion Window")
	defer window.Close()

	img := gocv.NewMat()
	defer img.Close()

	imgDelta := gocv.NewMat()
	defer imgDelta.Close()

	imgThresh := gocv.NewMat()
	defer imgThresh.Close()

	mog2 := gocv.NewBackgroundSubtractorMOG2()
	defer mog2.Close()

	for {
		buf := make([]byte, frameX*frameY*3)
		if _, err := io.ReadFull(out, buf); err != nil {
			done <- err
			continue
		}
		img, _ := gocv.NewMatFromBytes(frameY, frameX, gocv.MatTypeCV8UC3, buf)
		if img.Empty() {
			continue
		}

		var status CVStatus = Stale
		statusColor := color.RGBA{0, 255, 0, 0}

		// first phase of cleaning up image, obtain foreground only
		mog2.Apply(img, &imgDelta)

		// remaining cleanup of the image to use for finding contours.
		// first use threshold
		gocv.Threshold(imgDelta, &imgThresh, 25, 255, gocv.ThresholdBinary)

		// then dilate
		kernel := gocv.GetStructuringElement(gocv.MorphRect, image.Pt(3, 3))
		defer kernel.Close()
		gocv.Dilate(imgThresh, &imgThresh, kernel)

		// now find contours
		contours := gocv.FindContours(imgThresh, gocv.RetrievalExternal, gocv.ChainApproxSimple)

		for i := 0; i < contours.Size(); i++ {
			area := gocv.ContourArea(contours.At(i))
			if area < minArea {
				continue
			}

			status = Detected
			statusColor = color.RGBA{255, 0, 0, 0}
			gocv.DrawContours(&img, contours, i, statusColor, 2)

			rect := gocv.BoundingRect(contours.At(i))
			gocv.Rectangle(&img, rect, color.RGBA{0, 0, 255, 0}, 2)
		}

		gocv.PutText(&img, string(status), image.Pt(10, 20), gocv.FontHersheyPlain, 1.2, statusColor, 2)

		window.IMShow(img)
		if window.WaitKey(1) == 27 {
			break
		}
	}
	return done
}

func webrtc_conn(in io.Writer) error {
	ivfWriter, err := ivfwriter.NewWith(in)

	if err != nil {
		return err

	}

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	peerConn, err := webrtc.NewPeerConnection(config)

	if err != nil {
		return err
	}

	peerConn.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("Connection State has changed %s \n", connectionState.String())
	})

	peerConn.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Track has started, of type %d: %s \n", track.PayloadType(), track.Codec().MimeType)

		t := time.NewTicker(3 * time.Second)
		go func() {

			for range t.C {

				err := peerConn.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
				if err != nil {
					log.Println("[webrtc] error: ", err.Error())
				}
			}

		}()
		for {
			rtp, _, err := track.ReadRTP()

			if err != nil {
				panic(err)

			}

			if err := ivfWriter.WriteRTP(rtp); err != nil {
				panic(err)
			}
		}

	})

	offer := webrtc.SessionDescription{}
	err = stdin("Enter the session description", &offer)
	if err != nil {
		return err
	}
	err = peerConn.SetRemoteDescription(offer)
	if err != nil {
		return err
	}

	ans, err := peerConn.CreateAnswer(nil)
	if err != nil {
		return err
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConn)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConn.SetLocalDescription(ans)
	if err != nil {
		return err
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	// Output the answer in base64 so we can paste it in browser
	desc, err := json.Marshal(*peerConn.LocalDescription())
	if err != nil {
		return err
	}
	fmt.Println("[webrtc] local description: ", desc)
	return nil
}
