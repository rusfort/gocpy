# Gocpy logo v3: single snake line, infinity C->G, gradient along path.
import math, cairosvg
from PIL import Image, ImageFilter, ImageChops
W=64; R=170; C0=(300.0,512.0); G0=(724.0,512.0)
BAR_Y=530.0; BAR_X0=G0[0]-30; END_A=-62.0
STOPS=[(0.0,'#8A5CFF'),(0.22,'#7C4DFF'),(0.36,'#4D6BFF'),(0.50,'#19C8FF'),(0.62,'#19F0FF'),(0.76,'#2FA8FF'),(0.84,'#7C4DFF'),(0.93,'#C64DF0'),(1.0,'#FFB224')]
HEAD='#FFB224'; BG='#0B1020'; FRAME='#19F0FF'
def hx(c): return tuple(int(c[i:i+2],16) for i in (1,3,5))
def col(t):
    for (a,ca),(b,cb) in zip(STOPS,STOPS[1:]):
        if a<=t<=b:
            u=(t-a)/(b-a); A,B=hx(ca),hx(cb); return '#%02x%02x%02x'%tuple(round(A[i]+(B[i]-A[i])*u) for i in range(3))
    return STOPS[-1][1]
def P(c,deg): r=math.radians(deg); return (c[0]+R*math.cos(r), c[1]+R*math.sin(r))
mid=((C0[0]+G0[0])/2, C0[1]); dm=(G0[0]-C0[0])/2; phi=math.degrees(math.acos(R/dm))
pts=[]  # (x,y,seg)
def line(a,b,n,seg):
    for k in range(n): pts.append((a[0]+(b[0]-a[0])*k/n, a[1]+(b[1]-a[1])*k/n, seg))
def arc(c,a0,a1,n,seg):
    for k in range(n): x,y=P(c,a0+(a1-a0)*k/n); pts.append((x,y,seg))
a_bar=math.degrees(math.asin((BAR_Y-G0[1])/R))
line((BAR_X0,BAR_Y),(G0[0]+R*math.cos(math.radians(a_bar)),BAR_Y),80,0)
arc(G0,a_bar,180-phi,300,1)                    # G: right side, bottom, to lower-left tangent
line(P(G0,180-phi),P(C0,-phi),160,2)            # crossing 1 (under)
arc(C0,-phi,-360+phi,700,3)                     # C: top, left, bottom (ccw)
line(P(C0,phi),P(G0,180+phi),160,4)             # crossing 2 (over)
arc(G0,180+phi,360+END_A,180,5); x,y=P(G0,360+END_A); pts.append((x,y,5))
# arc-length param
L=[0.0]
for i in range(1,len(pts)): L.append(L[-1]+math.dist(pts[i][:2],pts[i-1][:2]))
T=[l/L[-1] for l in L]
over=[p for p in pts if p[2]==4]
def gapped(p):
    return p[2]==2 and min((p[0]-q[0])**2+(p[1]-q[1])**2 for q in over) < (W/2+16)**2
def mark_svg():
    A=P(C0,phi); B=P(G0,180+phi)
    ux,uy=B[0]-A[0],B[1]-A[1]; ln=math.hypot(ux,uy); nx,ny=-uy/ln*(W/2+16), ux/ln*(W/2+16)
    a=(A[0]+ux*0.22,A[1]+uy*0.22); b=(A[0]+ux*0.78,A[1]+uy*0.78)
    band=f"M{a[0]+nx:.1f},{a[1]+ny:.1f} L{b[0]+nx:.1f},{b[1]+ny:.1f} L{b[0]-nx:.1f},{b[1]-ny:.1f} L{a[0]-nx:.1f},{a[1]-ny:.1f} Z"
    out=[f'<defs><clipPath id="gap"><path clip-rule="evenodd" d="M-500,-500 H1600 V1600 H-500 Z {band}"/></clipPath></defs>']
    under=[]; over=[]; N=len(pts); step=3
    for i in range(0,N-1,step):
        j=min(i+step,N-1)
        el=f'<line x1="{pts[i][0]:.1f}" y1="{pts[i][1]:.1f}" x2="{pts[j][0]:.1f}" y2="{pts[j][1]:.1f}" stroke="{col(T[i])}" stroke-width="{W}" stroke-linecap="round"/>'
        (over if pts[i][2]>=4 else under).append(el)
    out.append('<g clip-path="url(#gap)">'+''.join(under)+'</g>')
    out.extend(over)
    # head
    ex,ey=pts[-1][:2]; th=math.radians(360+END_A); d=(-math.sin(th),math.cos(th)); ang=math.degrees(math.atan2(d[1],d[0]))
    ra,rb=58,40; cx,cy=ex+d[0]*22, ey+d[1]*22
    def e(t):
        a=math.radians(ang); return (cx+ra*math.cos(t)*math.cos(a)-rb*math.sin(t)*math.sin(a), cy+ra*math.cos(t)*math.sin(a)+rb*math.sin(t)*math.cos(a))
    # slightly pointed snout: superellipse-ish via cos power
    E=[e(2*math.pi*k/120) for k in range(120)]
    n=(-d[1],d[0])  # normal (upper side for rightward motion => negative y)
    if n[1]>0: n=(-n[0],-n[1])
    eyx,eyy=cx+d[0]*18+n[0]*12, cy+d[1]*18+n[1]*12
    hp='M'+' L'.join(f'{x:.1f},{y:.1f}' for x,y in E)+' Z '+f'M{eyx+9:.1f},{eyy:.1f} A9,9 0 1 0 {eyx-9:.1f},{eyy:.1f} A9,9 0 1 0 {eyx+9:.1f},{eyy:.1f} Z'
    out.append(f'<path d="{hp}" fill="{HEAD}" fill-rule="evenodd"/>')
    return '\n'.join(out)
xs=[p[0] for p in pts]; ys=[p[1] for p in pts]
BB=(min(xs)-W/2, min(ys)-W/2-30, max(xs)+W/2+60, max(ys)+W/2)
def wrap(scale, bg='', glow=False):
    cx,cy=(BB[0]+BB[2])/2,(BB[1]+BB[3])/2; s=scale*1024/(BB[2]-BB[0])
    flt='<defs><filter id="gl" x="-20%" y="-20%" width="140%" height="140%"><feGaussianBlur stdDeviation="10" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter></defs>' if glow else ''
    g=' filter="url(#gl)"' if glow else ''
    return f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024" width="1024" height="1024">{flt}{bg}<g{g} transform="translate(512 512) scale({s:.4f}) translate({-cx:.1f} {-cy:.1f})">{mark_svg()}</g></svg>'
tile=f'<rect x="40" y="40" width="944" height="944" rx="190" fill="{BG}" stroke="{FRAME}" stroke-width="20"/>'
files={}
files0={'gocpy_mark':wrap(0.9),'gocpy_mark_dark':wrap(0.82,f'<rect width="1024" height="1024" fill="{BG}"/>'),'gocpy_file_icon':wrap(0.70,tile)}
def addglow(png, strength=0.9, rad=14):
    im=Image.open(png).convert('RGBA'); b=im.filter(ImageFilter.GaussianBlur(rad))
    r,g,bl,a=b.split(); a=a.point(lambda v:int(v*strength)); b=Image.merge('RGBA',(r,g,bl,a))
    base=Image.new('RGBA',im.size,(0,0,0,0)); base.alpha_composite(b); base.alpha_composite(im); base.save(png)
for k,v in files.items():
    open(k+'.svg','w').write(v); cairosvg.svg2png(bytestring=v.encode(),write_to=k+'.png',output_width=1024,output_height=1024)
# neon variant with glow (PNG only, for README/banners)
v=wrap(0.70,tile); cairosvg.svg2png(bytestring=v.encode(),write_to='gocpy_file_icon_neon.png',output_width=1024,output_height=1024); addglow('gocpy_file_icon_neon.png')

# ---- brand pack outputs ----
import os
os.makedirs('png',exist_ok=True)
open('gocpy-mark.svg','w').write(wrap(0.94))
open('gocpy-mark-dark.svg','w').write(wrap(0.82,f'<rect width="1024" height="1024" fill="{BG}"/>'))
open('gocpy-icon.svg','w').write(wrap(0.70,tile))
# tile variant tuned for tiny sizes: thicker frame, larger mark
tile_s=f'<rect x="24" y="24" width="976" height="976" rx="200" fill="{BG}" stroke="{FRAME}" stroke-width="40"/>'
open('gocpy-icon-small.svg','w').write(wrap(0.80,tile_s))
def r(svg,out,size): cairosvg.svg2png(url=svg,write_to=out,output_width=size,output_height=size)
for s in (16,24,32,48):
    r('gocpy-icon-small.svg',f'png/gocpy-icon-{s}.png',s)
for s in (64,96,128,144,180,192,256,512,1024):
    r('gocpy-icon.svg',f'png/gocpy-icon-{s}.png',s)
for s in (256,512,1024):
    r('gocpy-mark.svg',f'png/gocpy-mark-{s}.png',s)
r('gocpy-icon.svg','png/gocpy-icon-neon-1024.png',1024); addglow('png/gocpy-icon-neon-1024.png')
