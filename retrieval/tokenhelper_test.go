package retrieval_test

import "github.com/xingjianluo/code-index-service-go/parser"

func tokenize(s string) []string { return parser.Tokenize(s) }
