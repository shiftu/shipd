package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
)

// StdioAdapter is a tiny REPL-style adapter useful for development: it reads
// a command per line from r and writes the reply to w.
type StdioAdapter struct {
	In     io.Reader
	Out    io.Writer
	Prompt string // displayed before each line; empty disables it
}

func (a *StdioAdapter) Name() string { return "stdio" }

func (a *StdioAdapter) Run(ctx context.Context, dispatch DispatchFn) error {
	br := bufio.NewReaderSize(a.In, 4096)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if a.Prompt != "" {
			_, _ = fmt.Fprint(a.Out, a.Prompt)
		}
		line, err := br.ReadString('\n')
		if line != "" {
			// Stream callback prints each progress line on its own row;
			// the final Reply.Text is appended after the loop returns.
			stream := func(text string) {
				_, _ = fmt.Fprintln(a.Out, text)
			}
			reply := dispatch(ctx, Message{Text: line, ChatID: "stdio", UserID: "local"}, stream)
			_, _ = fmt.Fprintln(a.Out, reply.Text)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
