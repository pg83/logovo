"""Usage and argument errors exit non-zero with a message."""

import lib


def test():
    def bad(*args, expected):
        result = lib.run(*args, check=False, timeout=10)
        assert result.returncode != 0, args
        assert expected in result.stderr, (args, result.stderr)

    bad(expected="Usage:")
    bad("unknown", expected="Usage:")
    bad("collect", expected="-store is required")
    bad("collect", "-listen", "127.0.0.1:1", "-store", "ftp://x", expected="dir:/path or s3://bucket")
    bad("merge", expected="-store is required")
    bad("index", expected="-store is required")
    bad("serve", expected="-store or -index")
    bad("search", "-api", "http://127.0.0.1:1", expected="query is required")
    bad("show", "-api", "http://127.0.0.1:1", "not-a-uuid", expected="session uuid")
    bad("scan", "-once", "-root", "gemini=/tmp", expected="claude=/dir or -root codex=/dir")
    bad("collect", "-listen", "127.0.0.1:1", "-store", "s3://bucket", expected="AWS_ACCESS_KEY_ID")


if __name__ == "__main__":
    test()
    print("ok")
