import build
import os

build.flags.allow({
    "race": {
        "descr": "build logovo with the Go race detector; run with `./build -Drace test`",
        "default": "",
    },
})

RACE = bool(build.flags.race)


def touch(path):
    return [
        "python3",
        "-c",
        f"from pathlib import Path; p=Path(r'{path}'); p.parent.mkdir(parents=True, exist_ok=True); p.touch()",
    ]


GO_SOURCES = [
    path for path in build.glob("$(S)/*.go")
    if not path.endswith("_test.go")
]
GO_INPUTS = [
    *GO_SOURCES,
    *build.glob("$(S)/web/*"),
    "$(S)/go.mod",
    "$(S)/go.sum",
]

GO_ENV = {
    "CGO_ENABLED": "1" if RACE else "0",
    "GOFLAGS": "-mod=readonly -buildvcs=false",
    "GOTOOLCHAIN": "local",
    "GOWORK": "off",
}

logovo = command(
    name="logovo",
    inputs=GO_INPUTS,
    outputs=["$(B)/bin/logovo"],
    cmd=[
        "go", "build",
        "-trimpath",
        "-buildvcs=false",
        *(["-race"] if RACE else []),
        "-o", "$(B)/bin/logovo",
        ".",
    ],
    cwd="$(S)",
    env=GO_ENV,
    descr="GO",
    color="cyan",
)

e2e_tests = []
for test_path in build.glob("$(S)/tst/test_*.py"):
    test_name = test_path.rsplit("/", 1)[-1][len("test_"):-len(".py")]
    test_stamp = f"$(B)/tests/{test_name}.stamp"
    env = {
        "LOGOVO_TEST_BINARY": logovo.outputs[0],
        "PYTHONDONTWRITEBYTECODE": "1",
    }

    if RACE:
        env["GORACE"] = "halt_on_error=1 atexit_sleep_ms=0"

    e2e_tests.append(command(
        name=f"e2e_{test_name}",
        inputs=[test_path, "$(S)/tst/lib.py"],
        outputs=[test_stamp],
        deps=[logovo],
        cmd=[
            ["python3", test_path],
            touch(test_stamp),
        ],
        cwd="$(S)",
        env=env,
        descr="EE",
        color="green",
    ))

group("install", logovo)
group("e2e", *e2e_tests)
group("test", *e2e_tests)
