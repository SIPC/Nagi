# MiSans 子集：只保留拉丁、常用标点、全角符号和 GB2312 的全部汉字，减小 APK 体积。
# 缺的字由系统字体补上。用法：python subset.py [MiSans 所在目录]（需要 fontTools：pip install fonttools）
# 字重对应（见 docs/android.md 视觉）：400 用 Normal，500 用 Regular，600 用 Medium。
import os
import sys
from pathlib import Path

from fontTools import subset

SRC = Path(sys.argv[1] if len(sys.argv) > 1 else os.path.expandvars(r"%LOCALAPPDATA%\Microsoft\Windows\Fonts"))
OUT = Path(__file__).resolve().parent.parent / "src" / "main" / "res" / "font"
FONTS = {"MiSans-Normal.otf": "misans_normal.otf", "MiSans-Regular.otf": "misans_regular.otf", "MiSans-Medium.otf": "misans_medium.otf"}


def gb2312() -> set[int]:
    # 解码 GB2312 的全部双字节编码（区 0xA1-0xF7，位 0xA1-0xFE），无效的跳过。
    chars = set()
    for hi in range(0xA1, 0xF8):
        for lo in range(0xA1, 0xFF):
            try:
                chars.add(ord(bytes([hi, lo]).decode("gb2312")))
            except UnicodeDecodeError:
                pass
    return chars


def unicodes() -> set[int]:
    cps = set(range(0x20, 0x7F))  # 基本拉丁
    cps |= set(range(0xA0, 0x100))  # 拉丁补充
    cps |= set(range(0x2000, 0x2070))  # 常用标点
    cps |= set(range(0x2190, 0x2200))  # 箭头（→）
    cps |= set(range(0x3000, 0x3040))  # 中日韩符号和标点
    cps |= set(range(0xFF00, 0xFFF0))  # 全角
    cps |= gb2312()
    return cps


def main() -> None:
    opts = subset.Options()
    opts.layout_features = ["tnum", "kern", "liga", "locl", "ccmp", "mark", "mkmk"]
    opts.hinting = False
    opts.desubroutinize = False
    opts.name_IDs = ["*"]
    opts.notdef_outline = True
    cps = unicodes()
    OUT.mkdir(parents=True, exist_ok=True)
    for src, dst in FONTS.items():
        font = subset.load_font(str(SRC / src), opts)
        s = subset.Subsetter(opts)
        s.populate(unicodes=cps)
        s.subset(font)
        subset.save_font(font, str(OUT / dst), opts)
        print(dst, (OUT / dst).stat().st_size)


if __name__ == "__main__":
    main()
