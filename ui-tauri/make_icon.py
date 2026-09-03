"""生成 LocalMap logo：渐变圆角方块 + 白色定位图钉（钉内圆孔），4x 超采样抗锯齿。"""
from PIL import Image, ImageDraw

S = 4          # 超采样倍数
SIZE = 1024    # 输出尺寸
W = SIZE * S

# 渐变端色：左上亮蓝 -> 右下深蓝
C1 = (79, 140, 255)    # #4f8cff
C2 = (43, 79, 160)     # #2b4fa0
RADIUS = 200 * S

img = Image.new("RGBA", (W, W), (0, 0, 0, 0))

# --- 对角线渐变 ---
grad = Image.new("RGB", (W, W))
gd = ImageDraw.Draw(grad)
for y in range(W):
    for_x_color = None
    # 按 (x+y) 对角线插值太慢，逐行插值即可（左->右 + 上->下 近似）
    t = y / W
    r = int(C1[0] + (C2[0] - C1[0]) * t)
    g = int(C1[1] + (C2[1] - C1[1]) * t)
    b = int(C1[2] + (C2[2] - C1[2]) * t)
    gd.line([(0, y), (W, y)], fill=(r, g, b))

# 圆角遮罩
mask = Image.new("L", (W, W), 0)
md = ImageDraw.Draw(mask)
md.rounded_rectangle([0, 0, W - 1, W - 1], radius=RADIUS, fill=255)
img.paste(grad, (0, 0), mask)

d = ImageDraw.Draw(img)

# --- 定位图钉：圆 + 下三角 ---
cx, cy = W // 2, int(W * 0.42)      # 圆心
pr = int(W * 0.185)                 # 圆半径
tip_y = int(W * 0.80)               # 尖端
half_w = int(pr * 0.86)             # 三角半宽（与圆自然衔接）

white = (255, 255, 255, 255)
d.ellipse([cx - pr, cy - pr, cx + pr, cy + pr], fill=white)
d.polygon([(cx - half_w, cy + int(pr * 0.45)),
           (cx + half_w, cy + int(pr * 0.45)),
           (cx, tip_y)], fill=white)

# --- 钉内圆孔（用渐变下半部颜色，形成镂空感）---
hr = int(pr * 0.42)
t = cy / W
hole = (int(C1[0] + (C2[0] - C1[0]) * t),
        int(C1[1] + (C2[1] - C1[1]) * t),
        int(C1[2] + (C2[2] - C1[2]) * t), 255)
d.ellipse([cx - hr, cy - hr, cx + hr, cy + hr], fill=hole)

# 缩小到目标尺寸（Lanczos）
img = img.resize((SIZE, SIZE), Image.LANCZOS)
img.save("icon-src.png")
print("saved icon-src.png", img.size)
