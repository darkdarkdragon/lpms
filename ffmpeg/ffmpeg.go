package ffmpeg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/golang/glog"
)

// #cgo pkg-config: libavformat libavfilter libavcodec libavutil libswscale gnutls
// #include <stdlib.h>
// #include "transcoder.h"
// #include "extras.h"
import "C"

var ErrTranscoderRes = errors.New("TranscoderInvalidResolution")
var ErrTranscoderHw = errors.New("TranscoderInvalidHardware")
var ErrTranscoderInp = errors.New("TranscoderInvalidInput")
var ErrTranscoderVid = errors.New("TranscoderInvalidVideo")
var ErrTranscoderStp = errors.New("TranscoderStopped")
var ErrTranscoderFmt = errors.New("TranscoderUnrecognizedFormat")
var ErrTranscoderPrf = errors.New("TranscoderUnrecognizedProfile")
var ErrTranscoderGOP = errors.New("TranscoderInvalidGOP")
var ErrTranscoderDev = errors.New("TranscoderIncompatibleDevices")
var ErrEmptyData = errors.New("EmptyData")
var ErrDNNInitialize = errors.New("TranscoderInvalidDNNProfile")

type Acceleration int

const (
	Software Acceleration = iota
	Nvidia
	Amd
)

type ComponentOptions struct {
	Name string
	Opts map[string]string
}

type Transcoder struct {
	handle  *C.struct_transcode_thread
	stopped bool
	started bool
	mu      *sync.Mutex
}

type TranscodeOptionsIn struct {
	Fname       string
	Accel       Acceleration
	Device      string
	Transmuxing bool
}

type TranscodeOptions struct {
	Oname    string
	Profile  VideoProfile
	Detector DetectorProfile
	Accel    Acceleration
	Device   string

	Muxer        ComponentOptions
	VideoEncoder ComponentOptions
	AudioEncoder ComponentOptions
}

type MediaInfo struct {
	Frames     int
	Pixels     int64
	DetectData DetectData
}

type TranscodeResults struct {
	Decoded MediaInfo
	Encoded []MediaInfo
}

// HasZeroVideoFrame opens video file and returns true if it has video stream with 0-frame
func HasZeroVideoFrame(fname string) bool {
	cfname := C.CString(fname)
	defer C.free(unsafe.Pointer(cfname))
	return int(C.lpms_is_bypass_needed(cfname)) == 1
}

// HasZeroVideoFrameBytes  opens video and returns true if it has video stream with 0-frame
func HasZeroVideoFrameBytes(data []byte) (bool, error) {
	if len(data) == 0 {
		return false, ErrEmptyData
	}
	or, ow, err := os.Pipe()
	if err != nil {
		return false, err
	}
	fname := fmt.Sprintf("pipe:%d", or.Fd())
	cfname := C.CString(fname)
	defer C.free(unsafe.Pointer(cfname))
	go func() {
		br := bytes.NewReader(data)
		io.Copy(ow, br)
		ow.Close()
	}()
	bres := int(C.lpms_is_bypass_needed(cfname))
	ow.Close()
	return bres == 1, nil
}

func RTMPToHLS(localRTMPUrl string, outM3U8 string, tmpl string, seglen_secs string, seg_start int) error {
	inp := C.CString(localRTMPUrl)
	outp := C.CString(outM3U8)
	ts_tmpl := C.CString(tmpl)
	seglen := C.CString(seglen_secs)
	segstart := C.CString(fmt.Sprintf("%v", seg_start))
	ret := int(C.lpms_rtmp2hls(inp, outp, ts_tmpl, seglen, segstart))
	C.free(unsafe.Pointer(inp))
	C.free(unsafe.Pointer(outp))
	C.free(unsafe.Pointer(ts_tmpl))
	C.free(unsafe.Pointer(seglen))
	C.free(unsafe.Pointer(segstart))
	if ret != 0 {
		glog.Infof("RTMP2HLS Transmux Return : %v\n", Strerror(ret))
		return ErrorMap[ret]
	}
	return nil
}

func Transcode(input string, workDir string, ps []VideoProfile) error {

	opts := make([]TranscodeOptions, len(ps))
	for i, param := range ps {
		oname := path.Join(workDir, fmt.Sprintf("out%v%v", i, filepath.Base(input)))
		opt := TranscodeOptions{
			Oname:   oname,
			Profile: param,
			Accel:   Software,
		}
		opts[i] = opt
	}
	inopts := &TranscodeOptionsIn{
		Fname: input,
		Accel: Software,
	}
	return Transcode2(inopts, opts)
}

func newAVOpts(opts map[string]string) *C.AVDictionary {
	var dict *C.AVDictionary
	for key, value := range opts {
		k := C.CString(key)
		v := C.CString(value)
		defer C.free(unsafe.Pointer(k))
		defer C.free(unsafe.Pointer(v))
		C.av_dict_set(&dict, k, v, 0)
	}
	return dict
}

// return encoding specific options for the given accel
func configAccel(inAcc, outAcc Acceleration, inDev, outDev string) (string, string, error) {
	switch inAcc {
	case Software:
		switch outAcc {
		case Software:
			return "libx264", "scale", nil
		case Nvidia:
			upload := "hwupload_cuda"
			if outDev != "" {
				upload = upload + "=device=" + outDev
			}
			return "h264_nvenc", upload + ",scale_cuda", nil
		}
	case Nvidia:
		switch outAcc {
		case Software:
			return "libx264", "scale_cuda", nil
		case Nvidia:
			// If we encode on a different device from decode then need to transfer
			if outDev != "" && outDev != inDev {
				return "", "", ErrTranscoderDev // XXX not allowed
			}
			return "h264_nvenc", "scale_cuda", nil
		}
	}
	return "", "", ErrTranscoderHw
}
func accelDeviceType(accel Acceleration) (C.enum_AVHWDeviceType, error) {
	switch accel {
	case Software:
		return C.AV_HWDEVICE_TYPE_NONE, nil
	case Nvidia:
		return C.AV_HWDEVICE_TYPE_CUDA, nil

	}
	return C.AV_HWDEVICE_TYPE_NONE, ErrTranscoderHw
}

func Transcode2(input *TranscodeOptionsIn, ps []TranscodeOptions) error {
	_, err := Transcode3(input, ps)
	return err
}

func Transcode3(input *TranscodeOptionsIn, ps []TranscodeOptions) (*TranscodeResults, error) {
	t := NewTranscoder()
	defer t.StopTranscoder()
	return t.Transcode(input, ps)
}

func (t *Transcoder) Transcode(input *TranscodeOptionsIn, ps []TranscodeOptions) (*TranscodeResults, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.handle == nil {
		return nil, ErrTranscoderStp
	}
	if input == nil {
		return nil, ErrTranscoderInp
	}
	hw_type, err := accelDeviceType(input.Accel)
	if err != nil {
		return nil, err
	}
	fname := C.CString(input.Fname)
	defer C.free(unsafe.Pointer(fname))
	if input.Transmuxing {
		t.started = true
	}
	if !t.started {
		ret := int(C.lpms_is_bypass_needed(fname))
		if ret != 1 {
			// Stream is either OK or completely broken, let the transcoder handle it
			t.started = true
		} else {
			// Audio-only segment, fail fast right here as we cannot handle them nicely
			return nil, ErrTranscoderVid
		}
	}
	params := make([]C.output_params, len(ps))
	for i, p := range ps {
		if p.Detector != nil {
			// We don't do any encoding for detector profiles
			// Adding placeholder values to pass checks for these everywhere
			p.Oname = "/dev/null"
			p.Profile = P144p30fps16x9
			p.Muxer = ComponentOptions{Name: "mpegts"}
		}
		oname := C.CString(p.Oname)
		defer C.free(unsafe.Pointer(oname))

		param := p.Profile
		w, h, err := VideoProfileResolution(param)
		if err != nil {
			if p.VideoEncoder.Name != "drop" && p.VideoEncoder.Name != "copy" {
				return nil, err
			}
		}
		br := strings.Replace(param.Bitrate, "k", "000", 1)
		bitrate, err := strconv.Atoi(br)
		if err != nil {
			if p.VideoEncoder.Name != "drop" && p.VideoEncoder.Name != "copy" {
				return nil, err
			}
		}
		encoder, scale_filter := p.VideoEncoder.Name, "scale"
		if encoder == "" {
			encoder, scale_filter, err = configAccel(input.Accel, p.Accel, input.Device, p.Device)
			if err != nil {
				return nil, err
			}
		}
		// preserve aspect ratio along the larger dimension when rescaling
		var filters string
		filters = fmt.Sprintf("%s='w=if(gte(iw,ih),%d,-2):h=if(lt(iw,ih),%d,-2)'", scale_filter, w, h)
		if input.Accel != Software && p.Accel == Software {
			// needed for hw dec -> hw rescale -> sw enc
			filters = filters + ",hwdownload,format=nv12"
		}
		// set FPS denominator to 1 if unset by user
		if param.FramerateDen == 0 {
			param.FramerateDen = 1
		}
		// Add fps filter *after* scale filter because otherwise we could
		// be scaling duplicate frames unnecessarily. This becomes a DoS vector
		// when a user submits two frames that are "far apart" in pts and
		// the fps filter duplicates frames to fill out the difference to maintain
		// a consistent frame rate.
		// Once we allow for alternating segments, this issue should be mitigated
		// and the fps filter can come *before* the scale filter to minimize work
		// when going from high fps to low fps (much more common when transcoding
		// than going from low fps to high fps)
		var fps C.AVRational
		if param.Framerate > 0 {
			filters += fmt.Sprintf(",fps=%d/%d", param.Framerate, param.FramerateDen)
			fps = C.AVRational{num: C.int(param.Framerate), den: C.int(param.FramerateDen)}
		}
		// if has a detector profile, ignore all video options
		if p.Detector != nil {
			deviceid := "0"
			// FIXME: Hardcoded DNN filter device to 0 for now
			//if input.Accel != Software && len(input.Device) > 0 {
			//deviceid = input.Device
			//}
			switch p.Detector.Type() {
			case SceneClassification:
				detectorProfile := p.Detector.(*SceneClassificationProfile)
				filters = fmt.Sprintf("lvpdnn=filter_type=lvpclassify:device=%s:model=%s:input=%s:output=%s:sample=%d",
					deviceid, detectorProfile.ModelPath, detectorProfile.Input, detectorProfile.Output, detectorProfile.SampleRate)
			}
		}
		var muxOpts C.component_opts
		var muxName string
		switch p.Profile.Format {
		case FormatNone:
			muxOpts = C.component_opts{
				// don't free this bc of avformat_write_header API
				opts: newAVOpts(p.Muxer.Opts),
			}
			muxName = p.Muxer.Name
		case FormatMPEGTS:
			muxName = "mpegts"
		case FormatMP4:
			muxName = "mp4"
			muxOpts = C.component_opts{
				opts: newAVOpts(map[string]string{"movflags": "faststart"}),
			}
		default:
			return nil, ErrTranscoderFmt
		}
		if muxName != "" {
			muxOpts.name = C.CString(muxName)
			defer C.free(unsafe.Pointer(muxOpts.name))
		}
		// Set video encoder options
		if len(p.VideoEncoder.Name) <= 0 && len(p.VideoEncoder.Opts) <= 0 {
			p.VideoEncoder.Opts = map[string]string{
				"forced-idr": "1",
			}
			switch p.Profile.Profile {
			case ProfileH264Baseline:
				p.VideoEncoder.Opts["profile"] = ProfileParameters[p.Profile.Profile]
			case ProfileH264Main, ProfileH264High:
				p.VideoEncoder.Opts["profile"] = ProfileParameters[p.Profile.Profile]
				p.VideoEncoder.Opts["bf"] = "3"
			case ProfileH264ConstrainedHigh:
				p.VideoEncoder.Opts["profile"] = ProfileParameters[p.Profile.Profile]
				p.VideoEncoder.Opts["bf"] = "0"
			case ProfileNone:
				// Do nothing, the encoder will use default profile
			default:
				return nil, ErrTranscoderPrf
			}
		}
		gopMs := 0
		if param.GOP != 0 {
			if param.GOP <= GOPInvalid {
				return nil, ErrTranscoderGOP
			}
			// Check for intra-only
			if param.GOP == GOPIntraOnly {
				p.VideoEncoder.Opts["g"] = "0"
			} else {
				if param.Framerate > 0 {
					gop := param.GOP.Seconds()
					interval := strconv.Itoa(int(gop * float64(param.Framerate)))
					p.VideoEncoder.Opts["g"] = interval
				} else {
					gopMs = int(param.GOP.Milliseconds())
				}
			}
		}
		vidOpts := C.component_opts{
			name: C.CString(encoder),
			opts: newAVOpts(p.VideoEncoder.Opts),
		}
		audioEncoder := p.AudioEncoder.Name
		if audioEncoder == "" {
			audioEncoder = "aac"
		}
		audioOpts := C.component_opts{
			name: C.CString(audioEncoder),
			opts: newAVOpts(p.AudioEncoder.Opts),
		}
		vfilt := C.CString(filters)
		defer C.free(unsafe.Pointer(vidOpts.name))
		defer C.free(unsafe.Pointer(audioOpts.name))
		defer C.free(unsafe.Pointer(vfilt))
		params[i] = C.output_params{fname: oname, fps: fps,
			w: C.int(w), h: C.int(h), bitrate: C.int(bitrate),
			gop_time: C.int(gopMs),
			muxer:    muxOpts, audio: audioOpts, video: vidOpts, vfilters: vfilt}
		defer func(param *C.output_params) {
			// Work around the ownership rules:
			// ffmpeg normally takes ownership of the following AVDictionary options
			// However, if we don't pass these opts to ffmpeg, then we need to free
			if param.muxer.opts != nil {
				C.av_dict_free(&param.muxer.opts)
			}
			if param.audio.opts != nil {
				C.av_dict_free(&param.audio.opts)
			}
			if param.video.opts != nil {
				C.av_dict_free(&param.video.opts)
			}
		}(&params[i])
	}
	var device *C.char
	if input.Device != "" {
		device = C.CString(input.Device)
		defer C.free(unsafe.Pointer(device))
	}
	inp := &C.input_params{fname: fname, hw_type: hw_type, device: device,
		handle: t.handle}
	if input.Transmuxing {
		inp.transmuxe = 1
	}
	results := make([]C.output_results, len(ps))
	decoded := &C.output_results{}
	var (
		paramsPointer  *C.output_params
		resultsPointer *C.output_results
	)
	if len(params) > 0 {
		paramsPointer = (*C.output_params)(&params[0])
		resultsPointer = (*C.output_results)(&results[0])
	}
	ret := int(C.lpms_transcode(inp, paramsPointer, resultsPointer, C.int(len(params)), decoded))
	if ret != 0 {
		glog.Error("Transcoder Return : ", ErrorMap[ret])
		return nil, ErrorMap[ret]
	}
	tr := make([]MediaInfo, len(ps))
	for i, r := range results {
		tr[i] = MediaInfo{
			Frames: int(r.frames),
			Pixels: int64(r.pixels),
		}
		// add detect result
		if ps[i].Detector != nil {
			switch ps[i].Detector.Type() {
			case SceneClassification:
				detector := ps[i].Detector.(*SceneClassificationProfile)
				res := make(SceneClassificationData)
				for j, class := range detector.Classes {
					res[class.ID] = float64(r.probs[j])
				}
				tr[i].DetectData = res
			}
		}
	}
	dec := MediaInfo{
		Frames: int(decoded.frames),
		Pixels: int64(decoded.pixels),
	}
	return &TranscodeResults{Encoded: tr, Decoded: dec}, nil
}

func (t *Transcoder) Discontinuity() {
	t.mu.Lock()
	defer t.mu.Unlock()
	C.lpms_transcode_discontinuity(t.handle)
}

func NewTranscoder() *Transcoder {
	return &Transcoder{
		handle: C.lpms_transcode_new(),
		mu:     &sync.Mutex{},
	}
}

func (t *Transcoder) StopTranscoder() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	C.lpms_transcode_stop(t.handle)
	t.handle = nil // prevent accidental reuse
	t.stopped = true
}

type LogLevel C.enum_LPMSLogLevel

const (
	FFLogTrace   = C.LPMS_LOG_TRACE
	FFLogDebug   = C.LPMS_LOG_DEBUG
	FFLogVerbose = C.LPMS_LOG_VERBOSE
	FFLogInfo    = C.LPMS_LOG_INFO
	FFLogWarning = C.LPMS_LOG_WARNING
	FFLogError   = C.LPMS_LOG_ERROR
	FFLogFatal   = C.LPMS_LOG_FATAL
	FFLogPanic   = C.LPMS_LOG_PANIC
	FFLogQuiet   = C.LPMS_LOG_QUIET
)

func InitFFmpegWithLogLevel(level LogLevel) {
	C.lpms_init(C.enum_LPMSLogLevel(level))
}

func InitFFmpeg() {
	InitFFmpegWithLogLevel(FFLogWarning)
}

func ReleaseFFmpegDetectorProfile() {
	C.lpms_dnnrelease()
}
func InitFFmpegWithDetectorProfile(detector DetectorProfile, deviceids string) error {

	switch detector.Type() {
	case SceneClassification:
		detectorProfile := detector.(*SceneClassificationProfile)
		dnnOpt := &C.lvpdnn_opts{
			modelpath:  C.CString(detectorProfile.ModelPath),
			inputname:  C.CString(detectorProfile.Input),
			outputname: C.CString(detectorProfile.Output),
			deviceids:  C.CString(deviceids),
		}
		defer C.free(unsafe.Pointer(dnnOpt.modelpath))
		defer C.free(unsafe.Pointer(dnnOpt.inputname))
		defer C.free(unsafe.Pointer(dnnOpt.outputname))
		defer C.free(unsafe.Pointer(dnnOpt.deviceids))

		ret := int(C.lpms_dnninit(dnnOpt))
		if 0 != ret {
			glog.Error("lpms_dnninit Return : ", ErrorMap[ret])
			return ErrDNNInitialize
		}
	}

	InitFFmpegWithLogLevel(FFLogWarning)
	return nil
}
