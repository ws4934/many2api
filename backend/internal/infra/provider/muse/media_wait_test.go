package muse

import (
	"testing"
	"time"
)

func TestMediaTextWaitReportsAndRefusals(t *testing.T) {
	for _, tc := range []struct {
		text    string
		refusal bool
	}{
		{"Your video is ready.", false},
		{"The video has been generated successfully. Video not found", false},
		{"视频已生成，规格正好：720x1280 竖屏、时长 6 秒。 Video not found", false},
		{"视频生成好了，时长6秒。", false},
		{"已生成，等待附件加载。", false},
		{"verified generated artifact", false},
		{"Generating your video now.", false},
		{"Your image is ready.", false},
		{"视频没有生成成功，请重新尝试。", true},
		{"The video has not been generated successfully. Please try again.", true},
		{"Your video hasn't been generated. Please try again.", true},
		{"The video could not be generated.", true},
		{"The video couldn't be generated.", true},
		{"No video has been generated.", true},
		{"The video was generated, but the upload failed.", true},
		{"视频已生成，但随后处理失败，无法提供视频。", true},
		{"I can’t help with that.", true},
		{"Here is a description instead.", true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			var wait mediaTextWait
			now := time.Now()
			st := mediaState{chatState: chatState{MD: tc.text}}
			if wait.textOnly(st, now) || wait.textOnly(st, now.Add(14*time.Second)) {
				t.Fatal("rejected before the quiet period")
			}
			for _, delay := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute} {
				if got := wait.textOnly(st, now.Add(delay)); got != tc.refusal {
					t.Fatalf("after %s: refusal=%v, want %v", delay, got, tc.refusal)
				}
			}
		})
	}
}

func TestMediaTextWaitActivityAndLaterFailure(t *testing.T) {
	now := time.Now()
	var wait mediaTextWait
	st := mediaState{chatState: chatState{MD: "Your video is ready."}}
	wait.textOnly(st, now)
	st.MD = "The upload failed."
	if wait.textOnly(st, now.Add(30*time.Second)) {
		t.Fatal("new text was not given a quiet period")
	}
	st.Stop = true
	if wait.textOnly(st, now.Add(time.Minute)) {
		t.Fatal("stopped while streaming")
	}
	st.Stop, st.Pending = false, 1
	if wait.textOnly(st, now.Add(90*time.Second)) {
		t.Fatal("stopped while attachment pending")
	}
	st.Pending = 0
	if !wait.textOnly(st, now.Add(105*time.Second)) {
		t.Fatal("explicit failure was not returned")
	}
}
