package utility

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"zsearch/indexer/model"

	"github.com/blevesearch/bleve/v2"
)

func OpenOrCreateIndex(indexPath string) (bleve.Index, error) {
	index, err := bleve.Open(indexPath)
	if err == bleve.ErrorIndexPathDoesNotExist {
		indexMapping := bleve.NewIndexMapping()
		index, err = bleve.New(indexPath, indexMapping)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return index, nil
}

func IndexFiles(index bleve.Index, files []model.FileInfo) error {
	for _, file := range files {
		fmt.Printf("bleve indexing file %s\n", file.Path)
		err := index.Index(file.Path, file.Content+" "+file.Filename)
		if err != nil {
			return err
		}
	}
	return nil
}

func CleanText(input string) string {
	// 1. Remove new lines
	input = strings.ReplaceAll(input, "\n", " ")

	// 2. Remove commas and special characters
	input = regexp.MustCompile(`[^\w\s]`).ReplaceAllString(input, "")

	// 3. Convert to lower case for uniformity
	input = strings.ToLower(input)

	// 4. Remove stop words using the stopwords library
	//cleanedText := stopwords.CleanString(input, "en", true)
	cleanedText := input
	// 5. Return the cleaned result
	return cleanedText
}

func SizeOfIndex(path string) (int64, error) {
	var size int64

	err := filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})

	if err != nil {
		return 0, err
	}

	return size, nil
}
