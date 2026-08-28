package downloader

import (
	"context"
	"fmt"
	"os"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

// Download 通过 MTProto 下载文档到 dest（ToPath 内部处理 .part 临时文件）
// 返回实际下载字节数。
func Download(ctx context.Context, api *tg.Client, doc *tg.Document, dest string) (int64, error) {
	loc := &tg.InputDocumentFileLocation{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
	}
	d := downloader.NewDownloader()
	if _, err := d.Download(api, loc).WithThreads(4).ToPath(ctx, dest); err != nil {
		return 0, fmt.Errorf("下载失败: %w", err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// DownloadPhoto 下载图片（选择最大尺寸）到 dest
func DownloadPhoto(ctx context.Context, api *tg.Client, photo *tg.Photo, dest string) (int64, error) {
	best := bestPhotoSize(photo)
	if best == nil {
		return 0, fmt.Errorf("图片无可用尺寸")
	}
	loc := &tg.InputPhotoFileLocation{
		ID:            photo.ID,
		AccessHash:    photo.AccessHash,
		FileReference: photo.FileReference,
		ThumbSize:     best.Type,
	}
	d := downloader.NewDownloader()
	if _, err := d.Download(api, loc).WithThreads(4).ToPath(ctx, dest); err != nil {
		return 0, fmt.Errorf("下载失败: %w", err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// bestPhotoSize 取最大尺寸（优先 PhotoSize 类型，忽略缩略图 t/x 小尺寸）
func bestPhotoSize(photo *tg.Photo) *tg.PhotoSize {
	var best *tg.PhotoSize
	for _, s := range photo.Sizes {
		ps, ok := s.(*tg.PhotoSize)
		if !ok {
			continue
		}
		// 跳过微型缩略图（type 为单字符的）
		if len(ps.Type) == 1 {
			continue
		}
		if best == nil || ps.Size > best.Size {
			best = ps
		}
	}
	return best
}

// PhotoThumbType 返回最大尺寸的 type 标识（供下载用）
func PhotoThumbType(photo *tg.Photo) string {
	if b := bestPhotoSize(photo); b != nil {
		return b.Type
	}
	return "m"
}

// FileNameFromDoc 从文档 attributes 中提取文件名
func FileNameFromDoc(doc *tg.Document) string {
	for _, attr := range doc.Attributes {
		if f, ok := attr.(*tg.DocumentAttributeFilename); ok {
			return f.FileName
		}
	}
	return ""
}
