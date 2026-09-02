package bot

import (
	"testing"

	"github.com/gotd/td/tg"

	"tg-download-bot/internal/task"
)

func docOf(mime, name string) *tg.Document {
	d := &tg.Document{MimeType: mime}
	if name != "" {
		d.Attributes = []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: name},
		}
	}
	return d
}

func TestKindOfDoc(t *testing.T) {
	cases := []struct {
		name  string
		doc   *tg.Document
		attrs []tg.DocumentAttributeClass
		want  task.MediaKind
	}{
		{"视频属性", docOf("", ""), []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{}}, task.KindVideo},
		{"视频mime", docOf("video/mp4", "a.mp4"), nil, task.KindVideo},
		{"漫画cbz", docOf("", "海贼王 vol01.cbz"), nil, task.KindComic},
		{"漫画cbr大写", docOf("", "XX_001.CBR"), nil, task.KindComic},
		{"漫画mime", docOf("application/vnd.comicbook+zip", ""), nil, task.KindComic},
		{"电子书epub", docOf("", "book.epub"), nil, task.KindEbook},
		{"电子书mobi", docOf("", "book.mobi"), nil, task.KindEbook},
		{"电子书azw3", docOf("", "book.azw3"), nil, task.KindEbook},
		{"电子书txt", docOf("", "小说.txt"), nil, task.KindEbook},
		{"电子书txt mime", docOf("text/plain", ""), nil, task.KindEbook},
		{"pdf中性", docOf("application/pdf", "漫画.pdf"), nil, task.KindFile},
		{"音频", docOf("", "song.mp3"), []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{}}, task.KindFile},
		{"普通zip", docOf("", "data.zip"), nil, task.KindFile},
		{"图片文档", docOf("image/jpeg", "p001.jpg"), nil, task.KindFile},
	}
	for _, c := range cases {
		doc := c.doc
		if c.attrs != nil {
			doc.Attributes = c.attrs
		}
		if got := kindOfDoc(doc); got != c.want {
			t.Errorf("%s: kindOfDoc = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGuessExt(t *testing.T) {
	cases := map[string]string{
		"video/mp4":                     ".mp4",
		"video/x-matroska":              ".mkv",
		"application/epub+zip":          ".epub",
		"application/x-mobi8-ebook":     ".mobi",
		"application/vnd.comicbook+zip": ".cbz",
		"text/plain":                    ".txt",
		"application/pdf":               ".pdf",
		"image/jpeg":                    ".jpg",
		"unknown/weird":                 "",
	}
	for mime, want := range cases {
		if got := guessExt(mime); got != want {
			t.Errorf("guessExt(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestDescribeMedia(t *testing.T) {
	got := describeMedia(map[task.MediaKind]int{
		task.KindVideo: 1,
		task.KindPhoto: 3,
		task.KindComic: 2,
		task.KindEbook: 4,
		task.KindFile:  5,
	})
	want := "🎬 视频 1 个 🖼 图片 3 张 📖 漫画 2 个 📚 电子书 4 个 📄 文件 5 个"
	if got != want {
		t.Errorf("describeMedia = %q, want %q", got, want)
	}
}
