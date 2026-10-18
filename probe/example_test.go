package probe_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nrynss/keel/ffmpeg"
	"github.com/nrynss/keel/probe"
)

// stubTools writes stand-in ffprobe and ffmpeg scripts into dir, so the
// examples run on a host with no ffmpeg installed. The ffprobe stub answers
// the packet scan with three evenly spaced frames and any other call with a
// report of a rotated phone clip; the ffmpeg stub prints two cuts the way
// the metadata filter does.
func stubTools(dir string) (ffmpeg.Tools, error) {
	probeScript := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *packet=*) printf '0,0,0\\n0,512,512\\n0,1024,1024\\n' ;;\n" +
		"  *) cat <<'JSON'\n" +
		`{"streams":[` +
		`{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080,` +
		`"avg_frame_rate":"30/1","time_base":"1/15360","duration":"4.000000",` +
		`"side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]},` +
		`{"index":1,"codec_type":"audio","codec_name":"aac","sample_rate":"48000","channels":2,` +
		`"channel_layout":"stereo","bit_rate":"128000","duration":"4.000000"}],` +
		`"format":{"duration":"4.000000","bit_rate":"5000000"}}` + "\n" +
		"JSON\n" +
		"  ;;\n" +
		"esac\n"
	ffmpegScript := "#!/bin/sh\n" +
		"printf 'frame:0    pts:15360   pts_time:1\\nlavfi.scene_score=0.53\\n'\n" +
		"printf 'frame:1    pts:38400   pts_time:2.5\\nlavfi.scene_score=0.89\\n'\n"
	tools := ffmpeg.Tools{FFprobe: filepath.Join(dir, "ffprobe"), FFmpeg: filepath.Join(dir, "ffmpeg")}
	if err := os.WriteFile(tools.FFprobe, []byte(probeScript), 0o755); err != nil {
		return ffmpeg.Tools{}, err
	}
	if err := os.WriteFile(tools.FFmpeg, []byte(ffmpegScript), 0o755); err != nil {
		return ffmpeg.Tools{}, err
	}
	return tools, nil
}

// ExampleProbe reads a phone clip's streams. The picture is stored
// landscape with a quarter turn, so DisplaySize reports the portrait frame
// a player shows.
func ExampleProbe() {
	dir, err := os.MkdirTemp("", "keel-probe-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)
	tools, err := stubTools(dir)
	if err != nil {
		fmt.Println("write stubs failed:", err)
		return
	}

	media, err := probe.Probe(context.Background(), tools, filepath.Join(dir, "clip.mp4"))
	if err != nil {
		fmt.Println("probe failed:", err)
		return
	}
	video, _ := media.Video()
	audio, _ := media.Audio()
	w, h := video.DisplaySize()
	fmt.Println("duration:", media.Duration)
	fmt.Printf("video: %s %dx%d rotated %d, shown %dx%d at %s fps, variable %v\n",
		video.Codec, video.Width, video.Height, video.Rotation, w, h, video.FrameRate, video.VariableFrameRate)
	fmt.Printf("audio: %s %d Hz %s\n", audio.Codec, audio.SampleRate, audio.ChannelLayout)
	// Output:
	// duration: 4s
	// video: h264 1920x1080 rotated 90, shown 1080x1920 at 30/1 fps, variable false
	// audio: aac 48000 Hz stereo
}

// ExampleScenes finds the hard cuts in a clip at the default threshold.
func ExampleScenes() {
	dir, err := os.MkdirTemp("", "keel-probe-example-")
	if err != nil {
		fmt.Println("temp dir failed:", err)
		return
	}
	defer os.RemoveAll(dir)
	tools, err := stubTools(dir)
	if err != nil {
		fmt.Println("write stubs failed:", err)
		return
	}

	cuts, err := probe.Scenes(context.Background(), tools, filepath.Join(dir, "clip.mp4"), 0)
	if err != nil {
		fmt.Println("scenes failed:", err)
		return
	}
	fmt.Println(cuts)
	// Output:
	// [1s 2.5s]
}
