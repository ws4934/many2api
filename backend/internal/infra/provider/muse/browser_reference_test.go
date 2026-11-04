package muse

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Browser-only fixtures: no login, network request to Muse or generated media.
func TestLiveComposerScopesUploads(t *testing.T) {
	p := blankPage(t, liveEngine(t))
	ctx := context.Background()
	eval := func(expr string) {
		t.Helper()
		if err := p.cmd(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "awaitPromise": true}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("form=", legacy), func(t *testing.T) {
			tag := "section"
			if legacy {
				tag = "form"
			}
			html := `<main><div class="hatch-chat-groupable-bubble chat-user-bubble"><button id="history-remove" aria-label="Remove attachment"></button><input id="history-file" type="file"><div role="status"><img></div></div><` + tag + ` id="composer"><div id="previews"></div><div><textarea></textarea><button aria-label="Send">Send</button><input id="upload" type="file" multiple></div></` + tag + `></main>`
			eval(`document.body.innerHTML=` + mustJSON(html) + `;window.historyClicks=0;document.querySelector('#history-remove').onclick=()=>window.historyClicks++;
document.querySelector('#upload').onchange=()=>{document.querySelector('#previews').innerHTML='';for(const f of document.querySelector('#upload').files){const b=document.createElement('button');b.setAttribute('aria-label','Remove attachment');b.onclick=()=>b.remove();document.querySelector('#previews').append(b);}};`)
			var count int
			if err := p.js(ctx, "attachmentCount", &count); err != nil || count != 0 {
				t.Fatalf("history counted: %d %v", count, err)
			}
			files := []attachment{
				{Name: "image.png", MIME: "image/png", Data: []byte("image-fixture")},
				{Name: "reference.mp4", MIME: "video/mp4", Data: referenceMP4(6000, "isom")},
				{Name: "voice.wav", MIME: "audio/wav", Data: referenceWAV()},
				{Name: "report.docx", MIME: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Data: referenceDOCX(t)},
			}
			if err := p.attach(ctx, files); err != nil {
				t.Fatal(err)
			}
			if err := p.js(ctx, "attachmentCount", &count); err != nil || count != len(files) {
				t.Fatalf("uploaded count=%d %v", count, err)
			}
			// Read actual File bytes from the selected control, not only its count.
			var actual struct {
				Result struct {
					Value []string `json:"value"`
				} `json:"result"`
			}
			if err := p.cmd(ctx, "Runtime.evaluate", map[string]any{"expression": `Promise.all([...document.querySelector('#upload').files].map(async f=>btoa(String.fromCharCode(...new Uint8Array(await f.arrayBuffer())))))`, "awaitPromise": true, "returnByValue": true}, &actual); err != nil {
				t.Fatal(err)
			}
			if len(actual.Result.Value) != len(files) {
				t.Fatalf("file bytes=%v", actual.Result.Value)
			}
			for i, file := range files {
				if actual.Result.Value[i] != base64.StdEncoding.EncodeToString(file.Data) {
					t.Fatalf("file %s changed in upload", file.Name)
				}
			}
			eval(`document.querySelector('#previews').insertAdjacentHTML('beforeend','<div id="busy" role="status"><video></video></div>')`)
			if err := p.js(ctx, "attachmentCount", &count); err != nil || count != -1 {
				t.Fatalf("processing count=%d %v", count, err)
			}
			eval(`document.querySelector('#busy').remove()`)
			if err := p.attach(ctx, nil); err != nil {
				t.Fatal(err)
			}
			var untouched struct {
				Result struct {
					Value bool `json:"value"`
				} `json:"result"`
			}
			if err := p.cmd(ctx, "Runtime.evaluate", map[string]any{"expression": `window.historyClicks===0 && document.querySelector('#history-file').files.length===0 && document.querySelector('#previews').children.length===0`, "returnByValue": true}, &untouched); err != nil || !untouched.Result.Value {
				t.Fatalf("history changed or attachments retained: %+v %v", untouched, err)
			}
		})
	}
}

func TestLiveVideoAttachmentAfterReadyText(t *testing.T) {
	p := blankPage(t, liveEngine(t))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	expr := `document.body.innerHTML='<div class="hatch-chat-groupable-bubble chat-user-bubble"><p>generate</p></div><div class="hatch-chat-groupable-bubble hatch-agent-bubble-bg"><p>Your video is ready.</p></div>';setTimeout(()=>{const a=document.createElement('div');a.dataset.testid='hatch-chat-attachment-presentation-video';const v=document.createElement('video');v.src=URL.createObjectURL(new Blob(['generated-video'],{type:'video/mp4'}));a.append(v);document.body.append(a);},30000);`
	if err := p.cmd(ctx, "Runtime.evaluate", map[string]any{"expression": expr}, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	file, err := p.waitGeneratedMedia(ctx, mediaJob{Kind: mediaVideo}, nil, start)
	if err != nil || string(file.Data) != "generated-video" || !strings.HasPrefix(file.MIME, "video/") {
		t.Fatalf("late attachment=%q %s %v", file.Data, file.MIME, err)
	}
	t.Logf("delivered late video after %s", time.Since(start).Round(time.Millisecond))
}
