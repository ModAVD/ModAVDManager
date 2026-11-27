# 更新说明 / Release note lines

用户看得见的改动，在同一个 PR 里往这个文件夹加一个文件，写两行给用户看的话：第一行中文，第二行英文。ModAVD 发版时，工具把这个仓库上一版以后新加的文件汇总进 ModAVD 的发版公告：中文进分隔行以上的「更新内容」（同步到 Mirror 酱、在 AUTO-MAS 里显示），英文进分隔行以下、只在 GitHub 上显示的「What's new」，并在行尾补上这个 PR 的编号。不加文件的 PR 不会出现在公告里。

- **文件名**：`<名字>.<类型>.md`。名字用小写字母、数字、`-`、`_`，起一个不会和别的改动重复的（比如 issue 号加几个词：`210-resize-crash.fix.md`）。
- **类型**：`feat` 新增，`fix` 修复，`change` 变更，`remove` 移除。
- **内容**：正好两行，中间不空行，都不能有 `--`。写用户会看到的结果，不写原因和实现。
  - 第一行中文，不超过 60 字，比如「调高分辨率后开机不再卡死退出」。
  - 第二行英文，说同一件事，不超过 120 个字符，不能有中文字符，比如 `Starting no longer hangs after raising the resolution`。用词照[术语对照表](https://github.com/ModAVD/ModAVDManager/blob/main/internal/i18n/README.md#terms)。
  - 不会写中文的，第一行可以用机器翻译，在 PR 里说一声，由维护者在合并前改好。
- 想让公告指向 issue 而不是 PR，就自己在第一行行尾写上，issue 都在 ModAVD 仓库，要写全称：`……不再卡死退出 (ModAVD/ModAVD#210)`；这样工具不再补 PR 编号。英文那一行没写编号的，跟着用第一行的。
- 只和开发、测试、CI、文档有关的 PR 不用加。
- 已经发过版的文件不要改中文那一行、也不要重复用它的名字；以后的改动另起一个文件。只有英文那一行可以补或者改。

例子 `210-resize-crash.fix.md`：

```
调高分辨率后开机不再卡死退出 (ModAVD/ModAVD#210)
Starting no longer hangs after raising the resolution
```

这个仓库的 CI 不查这个文件夹，发版汇总时才查；格式不对会在那时一次全报出来。汇总的工具和完整的说明在 ModAVD 仓库：[changelog.d/README.md](https://github.com/ModAVD/ModAVD/blob/main/changelog.d/README.md)、[tools/pubrel/README.md](https://github.com/ModAVD/ModAVD/blob/main/tools/pubrel/README.md)。

---

A change that users can see adds one file here, in the same pull request, with two lines written for users: Chinese first, then English. When ModAVD is released, its tools collect the files this repository gained since the previous release into the ModAVD release notes: the Chinese lines go into 更新内容 above the separator line (synced to MirrorChyan and shown in AUTO-MAS), the English lines into "What's new" below it, which only GitHub shows; the pull request number is appended to each. A pull request without a file is not announced.

- **Name**: `<name>.<type>.md`, the name in lowercase letters, digits, `-`, `_`, and unique (for example the issue number and a few words: `210-resize-crash.fix.md`).
- **Type**: `feat` (新增), `fix` (修复), `change` (变更), `remove` (移除).
- **Content**: exactly two lines, no blank line between them, neither with `--`. Say what users will notice, not the cause or the implementation.
  - Line 1 in Chinese, at most 60 characters, for example 「调高分辨率后开机不再卡死退出」.
  - Line 2 in English, saying the same thing, at most 120 characters and without Chinese characters, for example `Starting no longer hangs after raising the resolution`. Use the words of the [table of terms](https://github.com/ModAVD/ModAVDManager/blob/main/internal/i18n/README.md#terms).
  - If you do not write Chinese, a machine translation is fine for line 1; say so in the pull request, and the maintainer will correct it before merging.
- To point at an issue instead of the pull request, end line 1 with it yourself, in full, since the issues are in the ModAVD repository: `…… (ModAVD/ModAVD#210)`. An English line without a number takes the one of line 1.
- Pull requests that only touch development, tests, CI or documentation need no file.
- Do not change the Chinese line of a file that has already been released, and do not reuse its name; a later change takes a new file. Only its English line may be added or changed.

Example `210-resize-crash.fix.md`:

```
调高分辨率后开机不再卡死退出 (ModAVD/ModAVD#210)
Starting no longer hangs after raising the resolution
```

The CI of this repository does not check this folder; the files are checked when they are collected at release time, and every problem is reported then. The collecting tools and the full description are in the ModAVD repository: [changelog.d/README.md](https://github.com/ModAVD/ModAVD/blob/main/changelog.d/README.md), [tools/pubrel/README.en.md](https://github.com/ModAVD/ModAVD/blob/main/tools/pubrel/README.en.md).
