# Generates pycases.json: what Python's json does with each input, the reference for jsonord's tests.
#
# Each input is bytes, decoded the way AUTO-MAS reads mas-avd.json (utf-8-sig), parsed with json.loads and written
# with json.dumps(ensure_ascii=False, indent=2). "out" is null when Python refuses the input.
#
#   python -I gen_pycases.py pycases.json
import base64
import json
import random
import struct
import sys

CASES = [
    "{}",
    "[]",
    '{"a": {}, "b": [], "c": [{}, []], "d": {"e": {"f": []}}}',
    '[[], {}, [[]], [{}], {"x": []}]',
    '{"名称": "魔改 AVD ✓ 😀", "x": "\\u00e9\\ud83d\\ude00", "": "empty key"}',
    '"\\u0000\\u0001\\u001f\\u007f\\b\\f\\n\\r\\t \\u0080 \\u00a0 \\u2028 \\u2029 \\ufffd \\ufeff"',
    '"a\\"b\\\\c\\/d"',
    '{"q": "say \\"hi\\"", "p": "C:\\\\sdk-src\\\\sdk-mas35\\\\", "u": "\\u4e2d\\u6587"}',
    "[6144, -1, 1.5, 1e3, 0.1, 123456789012345678901234567890, -0, -0.0, 0.0, 1E400, -1e400, 1e-400]",
    "[1.0, 100.0, 1e16, 1e15, 1e-5, 0.0001, 0.001, 2.5e-7, 1.7976931348623157e308, 5e-324, 2.2250738585072014e-308]",
    "[123.456e2, 0.30000000000000004, 1.23456789012345678, -12.5E+3, 9007199254740993, 9007199254740993.0]",
    "[1e22, 1e21, 1e-7, 123456789.123456789, 0.1e1, 10e-1, 1E+2, 1e-0, 4.35, 2.675, 1e23]",
    "[9999999999999998.0, 9999999999999999.0, 12345678901234567.0, 0.00001234, 0.0001234]",
    "[NaN, Infinity, -Infinity]",
    "[true, false, null]",
    "true",
    "null",
    "-0",
    "6144",
    '"just a string"',
    '{"a": 1, "b": 2, "a": 3}',
    '{"a": {"x": 1}, "b": 2, "a": [1]}',
    ' \t\r\n{ "a" :\r\n 1 ,"b":[ 1 ,2 ] } \n',
    "\ufeff" + '{"bom": true}',
    '{"a": [1, [2, [3, {"k": [4, {}]}]]]}',
    "[" * 100 + "]" * 100,
    "1" * 4300,
    '{"memoryMb": 6144, "balloon": true, "initErrors": [], "renderer": "GLES: Google (NVIDIA Corporation)"}',
    # errors
    "",
    " ",
    "{",
    "[",
    "]",
    "[1,]",
    '{"a":1,}',
    "{'a':1}",
    "[1 2]",
    "01",
    "1.",
    ".5",
    "-",
    "+1",
    "--1",
    "1e",
    "1e+",
    '"abc',
    '"a\nb"',
    '"a\tb"',
    '"\\x"',
    '"\\u12"',
    '"\\u12G4"',
    '"\\ud800\\u12G4"',
    "[1]x",
    "nul",
    "truex",
    '{"a" 1}',
    '{"a":}',
    "{1: 2}",
    "infinity",
    "nan",
    "-NaN",
    "\ufeff\ufeff{}",
    "[" * 5000 + "]" * 5000,
    "1" * 4301,
    "-" + "1" * 4301,
]

RAW_CASES = [
    b"\xff{}",
    b'{"a": "\xe4\xb8"}',
    b'{"a": "\xed\xa0\x80"}',
    b"\xef\xbb\xbf\xef\xbb\xbf[]",
]


def run(data: bytes):
    try:
        text = data.decode("utf-8-sig")
        return json.dumps(json.loads(text), ensure_ascii=False, indent=2)
    except Exception:
        return None


def main() -> None:
    inputs = [c.encode("utf-8") for c in CASES] + RAW_CASES
    cases = [
        {"in": base64.b64encode(b).decode("ascii"), "out": run(b)} for b in inputs
    ]
    # Random doubles: repr against Go's PythonFloat (passed as exact hex).
    rng = random.Random(20261028)
    floats = []
    for _ in range(3000):
        bits = rng.getrandbits(64)
        f = struct.unpack("<d", struct.pack("<Q", bits))[0]
        if f != f or f in (float("inf"), float("-inf")):
            continue
        floats.append({"hex": f.hex(), "repr": json.dumps(f)})
    for _ in range(1000):
        f = rng.uniform(-1e6, 1e6) if rng.random() < 0.5 else rng.random() * 10 ** rng.randint(-20, 25)
        floats.append({"hex": f.hex(), "repr": json.dumps(f)})
    with open(sys.argv[1], "w", encoding="ascii", newline="\n") as fh:
        json.dump({"cases": cases, "floats": floats}, fh, ensure_ascii=True, indent=0)
        fh.write("\n")


main()
