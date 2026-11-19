# 安卓组件库

安卓 App 的界面组件都在 `android/core/ui`（包 `ink.sipc.nagi.ui`），各页面只组合这些组件，不自己画按钮、行、提示。视觉规则和网页一致（[ui.md](ui.md)），页面怎么排见 [android.md](android.md#视觉)。

新页面先在这里找；没有合适的，先想是不是能用现有组件加一个参数，确实通用再加进 `core/ui` 并写进本文档。只在一个页面里用、和业务数据绑在一起的（例如首页的「需要处理」怎么从 `AttentionItem` 取颜色和按钮），留在页面里，用这里的组件拼。

## 基础

| 名称 | 文件 | 说明 |
|---|---|---|
| `NagiTheme` | Theme.kt | 包住整个 App；`NagiTheme.colors`、`NagiTheme.type` 取颜色和字体。跟随系统深浅色。 |
| `NagiColors` | Color.kt | 和网页同名的颜色：`background` `card` `muted` `border` `foreground` `mutedForeground` `mutedForegroundStrong` `primary` `primaryForeground` `destructive` `success` `warning` `warningText` `info` `brand`。不在页面里写色值。 |
| `NagiType` | Type.kt | MiSans。`body` 14/20，`bodyMedium` 14/20 中粗，`secondary` 13/18，`mono` 13/18 等宽，`section` 15/22 半粗，`topBar` 16/24 半粗，`keyNumber` 22/30，`value` 18/26，`button` 14/20，`small` 12/16。 |
| `Dimens` | Dimens.kt | 页面左右 20（`pageHorizontal`），区块间 16（`sectionGap`），区块标题高 36，行高 48 / 56（两行），紧凑行 44 / 52，按钮 36 / 32，图标按钮 48，胶囊 52，胶囊占的底部空白 104（`bottomFade`），顶部栏 56（`topBarHeight`，不含状态栏）。 |
| `Motion` | Motion.kt | 曲线（`ContentIn` `Enter` `Exit`）、时长（`FADE_IN` 160、`HEIGHT` 360、`ROLL` 260 等）和弹簧（`leadSpring` `trailSpring` `itemSpring`）。动画都从这里取，不在页面里写数字。 |
| `NagiIcons` | Icons.kt | 和网页同一套 Lucide 图标（线宽 2）；`NagiLogo` 是彩色标志。 |
| `formatRate` `formatBytes` | Format.kt | 速率、大小换成「数值 + 单位」，和网页的写法一致；带 `Text` 后缀的返回一个字符串。 |

## 顶栏和导航

- `NagiTopBar(title, onTitleClick, showChevron, actions)`：一级页面的顶栏，标题能点时（切换面板）后面带「⌄」。右边放 `NagiIconButton`。
- `NagiBackTopBar(title, onBack, subtitle, actions)`：二级页面的顶栏，返回、标题，后面可以跟一段等宽的副标题。
- `PillNav(items, selected, onSelect, orientation, header)`：悬浮的胶囊导航，横向（`PillOrientation.Horizontal`，底部）和竖向（`Vertical`，宽屏 600 dp 以上时竖在左侧 88 宽一栏的左下角）是同一个组件：外框、底色（`pillBackground`）、选中底色的滑动和拖尾、两端跟随回弹、长按放大和拖动切换（`PillGestures.kt`）都共用，改样式两个方向一起变。横向时选中的一项写出名称、宽度不跳变；竖向只有图标，拖动时选中的一项向右展开写出名称。`header` 放在各项前面、不可点（竖向放 logo）。`PillNavItem(icon, label, badge)`，`badge` 是红点。组件只画胶囊本身，位置由调用方定；底部时页面要留 `Dimens.bottomFade` 的空白，胶囊后面不加渐隐。
- `NagiTopBar(fade = true)`：内容从顶部栏下面滚过时用，顶部栏自己画背景，最下面 20 渐变成透明。内容铺满整页，上内边距是状态栏加 `Dimens.topBarHeight`。
- `UnderlineTabs(options, selected, onSelect)`：下划线样式的选择项（时间范围、视图），放在 `SectionHeader` 的右边。只换数据，不滑动下面的内容。网页对应的是 `Tabs` 的下划线样式。
- `PageTabs(options, selected, onSelect)`：页内标签（节点页的监控、容器，以后容器页的概览、日志……）。高 40，标签之间 24，下面一条横贯整页的细线（伸进页面左右的边距，所以放在页面内容的内边距外面，文字自己按 20 缩进），选中的一项加粗，下面 2 的下划线从上一项滑过去。下面的内容用 `PageTabContent(selected) { }` 包住，换标签时跟标签同方向滑入。

## 按钮

- `NagiButton(text, onClick, variant, size, enabled, loading)`：`ButtonVariant.Primary`（一页最多一个）、`Outline`（行内操作）、`Ghost`（重试、次要）、`Destructive`（删除、退出）；`ButtonSize.Default` 36、`Small` 32（行尾）。`loading` 时转圈、宽度不变。
- `NagiIconButton(icon, contentDescription, onClick)`：只有图标的按钮，可点区域 48。`contentDescription` 必填（读屏）。

## 区块

- `SectionHeader(title, trailing)`：区块标题行，右边放数量（`Roll`）、`UnderlineTabs`、「全部」。
- `SectionDivider()`：大区块之间的细线，上下各 16。网页上是卡片之间的间隔，App 上不用卡片。

## 列表行

可点的行一律用 `Modifier.rowClickable(onClick)`，不直接用 `clickable`：按下的底色左右各宽出 12（`RowInset`），圆角 8，内容位置不变。选中的一行传 `highlight`；列表外面包了 `SlidingSelection(rememberSelectionState())` 时再给每行一个 `selectionKey`，选中的底色由外面画，换一行时从上一行滑过去，这些行不再有按下的底色（宽屏设置菜单、节点列表）。`SelectionState.isBelowSelected(key)` 告诉右边的内容该往哪个方向滑。

- `ListRow(title, detail, dot, density, onClick, trailing)`：一行列表项。
  - `dot`：左边的状态点颜色（`success` 正常、`warning` 注意、`destructive` 严重、`info` 提示），两行时和第一行对齐。
  - `detail`：灰色的第二行（对象、时长），有它时行高变成两行的高度。
  - `trailing`：右边的控件（`NagiButton` 小号、数量）；没有控件但能点时自动显示「›」。
  - `density`：`RowDensity.Normal` 48 / 56，`RowDensity.Compact` 44 / 52（首页「需要处理」这种一屏要放多条的）。
- `MoreRow(text, onClick, density)`：列表折叠后的「还有 N 项」，文字和上面带点的行的标题对齐。
- `KeyValueRow(label, value, mono, labelWidth)`：名称和值，核对信息、详情页的属性。地址、指纹、ID 用 `mono`。
- `UsageRow(label, value, fraction, mono, color)`：名称、右边的数值，下面一条用量条。排行（流量最大的映射）、磁盘占用。

## 设置

- `SettingsGroup(title) { … }`：一组设置，上面是灰色的组标题；`title` 为 null 时只留间距。组之间 24，标题到第一行 4。
- `SettingsRow(title, onClick, value)`：一行设置，左边名称，右边当前值（灰色，太长截断）和「›」，点了进下一级。
- `ChoiceRow(title, selected, onClick)`：单选列表的一项（外观、语言），选中的右边打勾，读屏读作单选。

## 关键数字

- `StatValue(label, value, suffix, onClick)`：上面灰色名称，下面大数字和灰色的单位或分母（「12 / 14」「3.2 MB/s」）。两个一行，用 `Modifier.weight(1f)` 平分。能点时去对应的列表。

## 图表

- `LineChart(series, height, xLabels, tipTime, tipFormat)`：监控折线图，平滑曲线，不画网格和纵轴；`ChartLine(values, color, label)`。对外流量用 `brand`，节点之间用 `mutedForeground`。默认高 112。给了 `tipTime` 时可以看某一刻的数值：点一下看、再点收起，长按后左右拖动逐个看，接鼠标、触控板或用触控笔时悬浮就看；对准的时刻画竖的虚线，提示写时间和每条线的数值（靠右对齐），过了一半放到左边。
- `SankeyChart(rows, flows, view, texts)`：流量桑基图，和网页概览的「实时流量」同一套布局和动效。`SankeyRow` 是一行（节点、`SANKEY_EXTERNAL` 外部网络、面板），`SankeyFlow` 是一条路径（`FlowKind.External` 品牌色、`FlowKind.Mesh` 灰色，`relay` 画成虚线）；`view`（`SankeyView`）只决定显示哪些，`flows` 传全部，切换时图不重建、平滑换位。点一下线显示速率，接鼠标、触控板或用触控笔时悬浮就显示。
- `Modifier.mouseHover(onMove, onExit)`：只认鼠标（含触控板）的悬浮，手指照常点按、长按。图表的悬浮提示都用它。
- `ChartLegend(items)`：图例，放在图上面。
- `UsageBar(fraction, color, height)`：用量条，长度变化有过渡。
- `StatusDot(color, size)`：状态点，默认 6，颜色变化有过渡。
- `UsageRing(label, icon, fraction, track)`：一项占用的圆环（26，线宽 2.5，中间是图标，不写数字），首页的节点状态和节点列表都用它；到 80% 变警告色、90% 变红，不知道总量时传 null。放在 muted 底上（选中的一行）时 `track` 传 border。

## 头像和图标底

- `Avatar(name, image, size)`：成员头像，`image` 是面板给的图片字节；没有或解不开时是首字母加渐变底，和网页的默认头像一样。默认 40。
- `IconCircle(icon, size, iconSize)`：圆形 muted 底上的一个图标（解锁页、空状态的头图）。默认 40，图标 18。

- `ActionRow(title, description, actions)`：和网页的 `SettingRow` 一样，左边名称和灰色说明，右边最多两个小按钮，不整行可点。
- `SwitchRow(title, on, onToggle, labels, description, busy)`：开关写成设置行，第二行开头写「已开启」或「已关闭」，右边是「关闭」或「开启」按钮，不用安卓的开关控件。`labels` 是 `SwitchLabels`。
- 设置子页里的「选择一项」（网页的 `Select`）用 `feature/settings` 的 `SelectRow`：一行写当前的值，点了从底部弹出选项。

## 输入和对话框

- `NagiTextField(value, onValueChange, label, placeholder, error, maxLength, password)`：单行输入框，和网页的 Input 一样（高 40、圆角 6、1px 边框，获得焦点变深，出错变红）。`password` 时不显示内容、弹出密码键盘。
- `NagiDialog(title, confirm, cancel, onConfirm, onDismiss, text, danger, busy)`：确认框，「取消」和操作按钮并排各占一半；删除、退出、吊销用 `danger`（红色按钮）。`content` 放在说明下面（输入框、错误）。

## 提示和状态

- `Banner(title, lines, dot, action, onAction)`：页面顶部的通栏提示，muted 底，横向撑满。连不上面板、只读、版本过旧。`lines` 里的空字符串会跳过。
- `InlineError(message, retryLabel, onRetry, height)`：区块里加载失败，红字加「重试」。`height` 和加载成功时一样高，切换时不跳。
- `EmptyState(text, action)`：整页或整块没有内容，居中一行灰字，下面可以放一个按钮。
- `Skeleton(modifier)`、`SkeletonRows(rows)`：加载时的占位，尺寸和加载后的内容一样。
- 短暂提示用 Material 的 `Snackbar`，颜色用 `primary` / `primaryForeground`，放在胶囊上面。

## 动效

规则和网页一样（[ui.md](ui.md) 的「动画」）：有用途、简短、随时能打断；系统开了「移除动画」时（`isReducedMotion()`）位移和缩放归零，只剩淡入淡出。

- `Roll(text, style, color)`：跳字，数字变了只滚动变了的那几位，上下渐隐、略微模糊，不在行的边缘硬切。数量、速率、关键数字都用它。别的上下滚动的文字（例如竖向胶囊拖动时的名称）用同一套 `rollTransform`、`rollFadeEdges`、`rollBlur`。
- `Modifier.autoHeight()`：内容高度变化时过渡到新高度（条目增减）。
- `Collapse(visible) { … }`：整块展开、收起（提示条、折叠的内容）。
- `rememberSlidingIndicator(selected)`：选中指示器从上一项滑过去（`PillNav`、`UnderlineTabs` 用的就是它）。`tail()` 给出后面那一小段拖尾，`PillNav` 画成渐变（最长 20）。
- `barSpec()`、`animatedDotColor(color)`：用量条长度、状态点颜色的过渡。
- `NagiTransitions`：页面切换。`pushIn` `pushOut` `popIn` `popOut` 用在 `NavHost` 的四个 transition 上（进下一级从右边滑入，返回反过来）；`tab(forward, inPx, outPx, reduced)` 是主导航切换（同方向滑入 32、移开 16）。参数 `reduced` 传 `isReducedMotion()`。

## 预览

`Previews.kt` 里有一张整页的组件预览（浅色、深色），在 Android Studio 里看，对照 `android/design/s1.png`。改了组件的样子要看一眼这张预览。
