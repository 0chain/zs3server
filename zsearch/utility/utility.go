package utility

import (
	"fmt"
	"regexp"
	"strings"

	"zsearch/indexer/model"

	"github.com/bbalet/stopwords"
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
		fmt.Printf("indexing file %+v \n", file)
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
	cleanedText := stopwords.CleanString(input, "en", true)

	// 5. Return the cleaned result
	return cleanedText
}
