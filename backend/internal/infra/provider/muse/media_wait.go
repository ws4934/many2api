package muse

import (
	"regexp"
	"time"
)

// A delivery report is not a delivered file. Muse can finish its text well
// before the attachment is mounted, including after the Stop button vanishes.
var mediaReport = regexp.MustCompile(`(?i)\.(?:webp|png|jpe?g|mp4|webm|mov)\b|imagine_media|deliverable|artifact|verified\s+generated|` +
	`(?:视频|影片|短片|图片|图像)[^。！？!?\n]{0,40}(?:已(?:经)?(?:成功)?生成|生成(?:成功|好了|完毕|完成)|已(?:经)?完成|做好了|正在|处理中|加载)|` +
	`(?:已(?:经)?(?:成功)?生成|生成好了|生成完毕|已完成|做好了|正在生成|正在制作|正在渲染|等待附件|正在上传)|` +
	`\b(?:video|clip|image|picture|animation)\b[^.\n]{0,60}\b(?:generated|created|rendered|completed|ready|processing|loading)\b|` +
	`\b(?:generating|creating|rendering|uploading|generated|created|rendered|completed)\b[^.\n]{0,32}\b(?:video|clip|image|picture|animation)\b`)

// Failure takes precedence over an earlier success claim in the same reply.
var mediaFailureText = regexp.MustCompile(`(?i)(?:未|没(?:有)?|无法|不能|未能)[^。！？!?\n]{0,16}(?:生成|制作|提供|完成)|` +
	`(?:生成|制作|渲染|处理|上传)[^。！？!?\n]{0,12}(?:失败|未成功)|` +
	`\b(?:not|never|hasn['’]t|wasn['’]t|isn['’]t)\s+(?:(?:be|been|successfully)\s+){0,3}(?:generated|created|completed|ready)|` +
	`\b(?:no|without)\s+(?:video|clip|image|picture|animation)\b[^.\n]{0,60}\b(?:generated|created|ready)|` +
	`\b(?:failed|cannot|can['’]t|could not|couldn['’]t|unable to)\b[^.\n]{0,60}\b(?:generate|generated|generating|create|created|creating|render|provide|video|image|help)\b|` +
	`\b(?:video|image|render|upload)\b[^.\n]{0,32}\bfailed\b`)

type mediaTextWait struct {
	text    string
	changed time.Time
}

func (w *mediaTextWait) textOnly(st mediaState, now time.Time) bool {
	if st.MD != w.text || st.Stop || st.Pending > 0 {
		w.text, w.changed = st.MD, now
		return false
	}
	return w.text != "" && now.Sub(w.changed) >= 15*time.Second &&
		(mediaFailureText.MatchString(w.text) || !mediaReport.MatchString(w.text))
}
