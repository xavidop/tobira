package redis

import (
	"errors"
	"strings"
)

// splitCommand splits one command line into arguments the way redis-cli
// does, with shell-style joining:
//
//   - arguments are separated by spaces, tabs and newlines;
//   - "double quotes" understand \" \\ \n \r \t \b \a and \xHH;
//   - 'single quotes' are literal except for \' ;
//   - quoted and bare parts next to each other join into one argument.
//
// An empty line yields no arguments. An unterminated quote or a bad \x
// escape is an error.
func splitCommand(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inArg := false
	for i := 0; i < len(line); {
		ch := line[i]
		switch ch {
		case ' ', '\t', '\n', '\r':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
			i++
		case '"':
			inArg = true
			j, err := readDouble(line, i+1, &cur)
			if err != nil {
				return nil, err
			}
			i = j
		case '\'':
			inArg = true
			j, err := readSingle(line, i+1, &cur)
			if err != nil {
				return nil, err
			}
			i = j
		default:
			inArg = true
			cur.WriteByte(ch)
			i++
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args, nil
}

var errUnterminated = errors.New("unterminated quote")

// readDouble reads a double-quoted part starting after the opening quote and
// returns the index after the closing quote.
func readDouble(line string, i int, out *strings.Builder) (int, error) {
	for i < len(line) {
		ch := line[i]
		switch ch {
		case '"':
			return i + 1, nil
		case '\\':
			if i+1 >= len(line) {
				return 0, errUnterminated
			}
			e := line[i+1]
			switch e {
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case 'b':
				out.WriteByte('\b')
			case 'a':
				out.WriteByte('\a')
			case 'x':
				if i+3 >= len(line) || !isHex(line[i+2]) || !isHex(line[i+3]) {
					return 0, errors.New(`bad \x escape: want two hex digits`)
				}
				out.WriteByte(unhex(line[i+2])<<4 | unhex(line[i+3]))
				i += 2
			default:
				out.WriteByte(e)
			}
			i += 2
		default:
			out.WriteByte(ch)
			i++
		}
	}
	return 0, errUnterminated
}

// readSingle reads a single-quoted part starting after the opening quote.
func readSingle(line string, i int, out *strings.Builder) (int, error) {
	for i < len(line) {
		ch := line[i]
		switch {
		case ch == '\'':
			return i + 1, nil
		case ch == '\\' && i+1 < len(line) && line[i+1] == '\'':
			out.WriteByte('\'')
			i += 2
		default:
			out.WriteByte(ch)
			i++
		}
	}
	return 0, errUnterminated
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func unhex(b byte) byte {
	switch {
	case b >= 'a':
		return b - 'a' + 10
	case b >= 'A':
		return b - 'A' + 10
	}
	return b - '0'
}
