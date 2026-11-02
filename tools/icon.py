from PIL import Image, ImageDraw
S=1024
img=Image.new("RGBA",(S,S),(0,0,0,0))
# background gradient rounded square
bg=Image.new("RGBA",(S,S))
for y in range(S):
    t=y/S
    c=(int(30+(16-30)*t), int(136+(98-136)*t), int(229+(206-229)*t), 255)
    ImageDraw.Draw(bg).line([(0,y),(S,y)],fill=c)
mask=Image.new("L",(S,S),0); ImageDraw.Draw(mask).rounded_rectangle([40,40,S-40,S-40],radius=210,fill=255)
img.paste(bg,(0,0),mask)
d=ImageDraw.Draw(img)
W=(255,255,255,255); B=(22,110,214,255)
# NAS box (left)
d.rounded_rectangle([170,250,430,770],radius=40,fill=W)
for i,y in enumerate([340,450,560]):
    d.rounded_rectangle([215,y,385,y+60],radius=16,fill=B)
    d.ellipse([350,y+18,370,y+40],fill=(120,230,150,255))
# USB drive (right)
d.rounded_rectangle([640,300,820,350+380],radius=40,fill=W)
d.rounded_rectangle([672,205,788,305],radius=14,fill=W)
d.rectangle([697,235,717,265],fill=B); d.rectangle([743,235,763,265],fill=B)
d.ellipse([705,600,755,650],fill=B)
# arrows between
def arrow(y,left_to_right):
    x0,x1=465,605
    d.rounded_rectangle([x0,y-14,x1,y+14],radius=14,fill=W)
    if left_to_right: d.polygon([(x1+30,y),(x1-25,y-48),(x1-25,y+48)],fill=W)
    else: d.polygon([(x0-30,y),(x0+25,y-48),(x0+25,y+48)],fill=W)
arrow(440,True); arrow(600,False)
import sys,os
out=sys.argv[1]; os.makedirs(out,exist_ok=True)
for s in (16,24,32,48,64,72,256):
    img.resize((s,s),Image.LANCZOS).save(f"{out}/icon_{s}.png")
img.resize((64,64),Image.LANCZOS).save(f"{out}/PACKAGE_ICON.PNG")
img.resize((256,256),Image.LANCZOS).save(f"{out}/PACKAGE_ICON_256.PNG")
img.resize((128,128),Image.LANCZOS).save(f"{out}/web_icon.png")
