import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { inflateSync } from 'node:zlib';

import { afterAll, describe, expect, it } from 'vitest';

// 品牌资产回归：全套资产只有一档几何「轴 8 / 弧 6 / hub r=8 + 轴上下各 2px
// 出头」，不是任何一张好看的 512 预览。这里同时做源码结构断言与真实 16px
// 像素断言（有 rsvg-convert 时），防止下游把轴降到 5、把弧改成 5、把弧删成
// 闭合方框、把 hub 缩回 r=6、或把光学轴心挪到半像素。
const webDir = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..');
const read = (rel: string) => readFileSync(resolve(webDir, rel), 'utf8');

const favicon = read('public/favicon.svg');
const favicon16 = read('public/favicon-16.svg');
const logoMark = read('public/logo-mark.svg');
const logoMarkDark = read('public/logo-mark-dark.svg');
const brandMarkSource = read('src/components/BrandMark.tsx');

function expectA11y(svg: string, name: string): void {
  expect(svg, `${name} 必须有 role="img"`).toContain('role="img"');
  expect(svg, `${name} 必须有 <title>`).toMatch(/<title[^>]*>云枢 OpsMind<\/title>/);
  expect(svg, `${name} 必须声明 64×64 viewBox`).toContain('viewBox="0 0 64 64"');
}

function expectGeometry(svg: string, name: string): void {
  // 轴 8 / 弧 6：全套资产唯一档位，不许再分 5/6。
  expect(svg, `${name} 轴必须 stroke-width=8`).toMatch(/d="M24 8V56" stroke-width="8"/);
  expect(svg, `${name} 弧必须 stroke-width=6`).toMatch(
    /d="M36 16A20 20 0 0 1 36 48" stroke-width="6"/,
  );
  // 轴 y=8..56 vs 弧端 y=16/48：上下各 2px 出头。删掉这段就失去 16px 身份。
  expect(svg, `${name} 轴必须保留上下出头`).toContain('M24 8V56');
  expect(svg, `${name} 弧端点必须仍在 y=16/48`).toContain('M36 16A20 20 0 0 1 36 48');
  // 枢轴点
  expect(svg, `${name} 必须有枢轴点`).toMatch(/<circle cx="24" cy="32" r="8"/);
}

describe('brand mark assets', () => {
  it('favicon 带底板，不依赖宿主主题继承', () => {
    expectA11y(favicon, 'favicon.svg');
    expect(favicon).toContain('fill="#1e1e2e"');
    expect(favicon).toContain('stroke="#89b4fa"');
    expectGeometry(favicon, 'favicon.svg');
    // 光学轴心 x=28（几何 24 + translate 4）
    expect(favicon).toContain('translate(4 0)');
  });

  it('favicon-16 是带 crispEdges 的像素钉版，几何同为 6/8', () => {
    expectA11y(favicon16, 'favicon-16.svg');
    expect(favicon16).toContain('fill="#1e1e2e"');
    expect(favicon16).toContain('stroke="#89b4fa"');
    expectGeometry(favicon16, 'favicon-16.svg');
    expect(favicon16, '16px 钉版必须启用 crispEdges').toContain('shape-rendering="crispEdges"');
    expect(favicon16, '16px 钉版必须有光学补偿').toContain('translate(4 0)');
    // 16px 专用的 3px 圆角（favicon.svg 是 3.5px）：16px 角像素更干净。
    expect(favicon16, '16px 钉版底板圆角必须为 rx=12').toContain(
      '<rect width="64" height="64" rx="12"',
    );
  });

  it('logo-mark 是 ≥24px 的 6/8 档，浅/深各一份固定色', () => {
    expectA11y(logoMark, 'logo-mark.svg');
    expectGeometry(logoMark, 'logo-mark.svg');
    expect(logoMark).toContain('stroke="#1a56c4"');

    expectA11y(logoMarkDark, 'logo-mark-dark.svg');
    expectGeometry(logoMarkDark, 'logo-mark-dark.svg');
    expect(logoMarkDark).toContain('stroke="#89b4fa"');
  });

  it('内联组件保留 currentColor、role/title、统一 8/6 与光学补偿', () => {
    expect(brandMarkSource).toContain('currentColor');
    expect(brandMarkSource).toContain("role: 'img'");
    expect(brandMarkSource).toContain('strokeWidth={8}');
    expect(brandMarkSource).toContain('strokeWidth={6}');
    expect(brandMarkSource).not.toContain('arcStroke');
    expect(brandMarkSource).toContain('transform="translate(4 0)"');
    expect(brandMarkSource).toContain('r="8"');
    expect(brandMarkSource).toContain('<title');
  });
});

interface RgbaImage {
  width: number;
  height: number;
  data: Buffer;
}

/** rsvg-convert 输出 RGBA8 非隔行 PNG；只解这一种，避免引入图形依赖。 */
function decodePng(buf: Buffer): RgbaImage {
  let pos = 8;
  let width = 0;
  let height = 0;
  let bitDepth = 0;
  let colorType = 0;
  const idat: Buffer[] = [];
  while (pos + 8 <= buf.length) {
    const len = buf.readUInt32BE(pos);
    const type = buf.toString('ascii', pos + 4, pos + 8);
    const data = buf.subarray(pos + 8, pos + 8 + len);
    if (type === 'IHDR') {
      width = data.readUInt32BE(0);
      height = data.readUInt32BE(4);
      bitDepth = data[8] ?? 0;
      colorType = data[9] ?? 0;
      expect(bitDepth).toBe(8);
      expect(colorType).toBe(6);
      expect(data[12]).toBe(0); // 非隔行
    } else if (type === 'IDAT') {
      idat.push(data);
    } else if (type === 'IEND') {
      break;
    }
    pos += 12 + len;
  }
  expect(width).toBe(16);
  expect(height).toBe(16);

  const raw = inflateSync(Buffer.concat(idat));
  const bpp = 4;
  const stride = width * bpp;
  const out = Buffer.alloc(stride * height);
  let prev = Buffer.alloc(stride);
  for (let y = 0; y < height; y++) {
    const rowStart = y * (stride + 1);
    const filter = raw[rowStart] ?? 0;
    const line = raw.subarray(rowStart + 1, rowStart + 1 + stride);
    const cur = Buffer.alloc(stride);
    for (let x = 0; x < stride; x++) {
      const a = x >= bpp ? (cur[x - bpp] ?? 0) : 0;
      const b = prev[x] ?? 0;
      const c = x >= bpp ? (prev[x - bpp] ?? 0) : 0;
      let v = line[x] ?? 0;
      switch (filter) {
        case 0:
          break;
        case 1:
          v = (v + a) & 0xff;
          break;
        case 2:
          v = (v + b) & 0xff;
          break;
        case 3:
          v = (v + ((a + b) >> 1)) & 0xff;
          break;
        case 4: {
          const p = a + b - c;
          const pa = Math.abs(p - a);
          const pb = Math.abs(p - b);
          const pc = Math.abs(p - c);
          const pred = pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
          v = (v + pred) & 0xff;
          break;
        }
        default:
          throw new Error(`unsupported PNG filter ${filter}`);
      }
      cur[x] = v;
    }
    cur.copy(out, y * stride);
    prev = cur;
  }
  return { width, height, data: out };
}

function pixel(img: RgbaImage, x: number, y: number): number[] {
  const i = (y * img.width + x) * 4;
  return [img.data[i] ?? 0, img.data[i + 1] ?? 0, img.data[i + 2] ?? 0, img.data[i + 3] ?? 0];
}

function samePixel(a: number[], b: number[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

const hasRsvg = spawnSync('rsvg-convert', ['--version'], { stdio: 'ignore' }).status === 0;
const tmp = hasRsvg ? mkdtempSync(resolve(tmpdir(), 'opsmind-logo-')) : null;

afterAll(() => {
  if (tmp) {
    rmSync(tmp, { recursive: true, force: true });
  }
});

describe('16px pixel identity', () => {
  it.skipIf(!hasRsvg)('轴占两整列满色、弧 2px 满 alpha、hub 4px、轴上下有出头', () => {
    if (!tmp) {
      throw new Error('rsvg-convert 不可用');
    }
    const pngPath = resolve(tmp, 'favicon-16.png');
    execFileSync('rsvg-convert', [
      '-w',
      '16',
      '-h',
      '16',
      resolve(webDir, 'public/favicon-16.svg'),
      '-o',
      pngPath,
    ]);
    const img = decodePng(readFileSync(pngPath));
    const plate = [30, 30, 46, 255]; // #1e1e2e
    const mark = [137, 180, 250, 255]; // #89b4fa

    // 光学轴心 x=28 → 16px 的 7px；轴 stroke 8（2px）占第 6/7 列。
    // crispEdges 让这两列是满色，不带半透明边。
    for (const y of [3, 5, 7, 9, 12]) {
      expect(pixel(img, 6, y), `axis col6 row${y}`).toEqual(mark);
      expect(pixel(img, 7, y), `axis col7 row${y}`).toEqual(mark);
    }

    // 弧 6 档在 16px 是 1.5px；crispEdges 把它吸附成 2 列满 alpha（此前
    // 普通抗锯齿最淡只有 126/255）。row7 的弧在第 11/12 列，第 10 列是
    // 轴与弧之间的空档；row3 的弧上移，落在第 9/10 列。
    expect(pixel(img, 11, 7), 'arc row7 col11').toEqual(mark);
    expect(pixel(img, 12, 7), 'arc row7 col12').toEqual(mark);
    expect(pixel(img, 10, 7), 'arc gap row7 col10').toEqual(plate);
    expect(pixel(img, 9, 3), 'arc row3 col9').toEqual(mark);
    expect(pixel(img, 10, 3), 'arc row3 col10').toEqual(mark);
    expect(pixel(img, 11, 4), 'arc row4 col11').toEqual(mark);

    // hub r=8（轴宽 2×）→ 16px 直径 4px：中心行 row7/8 的 x5/x8 是
    // 近满 mark (125,163,228)；r=6 时只有 36% 混合 (69,84,120)，读成加粗轴。
    for (const y of [7, 8]) {
      for (const x of [5, 8]) {
        const p = pixel(img, x, y);
        expect(p[0], `hub col${x} row${y} R`).toBeGreaterThan(110);
        expect(p[1], `hub col${x} row${y} G`).toBeGreaterThan(140);
        expect(p[2], `hub col${x} row${y} B`).toBeGreaterThan(210);
        expect(p, `hub col${x} row${y} 不能退回底板`).not.toEqual(plate);
      }
      // hub 宽 4px：x4/x9 仍是底板，不能把 hub 做成方肩。
      expect(pixel(img, 4, y), `hub left bound row${y}`).toEqual(plate);
      expect(pixel(img, 9, y), `hub right bound row${y}`).toEqual(plate);
    }
    // hub 是圆点：row6/9 抗锯齿收窄（既非底板也非满色），row5/10 回到轴宽。
    for (const y of [6, 9]) {
      for (const x of [5, 8]) {
        const p = pixel(img, x, y);
        expect(p, `hub falloff col${x} row${y}`).not.toEqual(plate);
        expect(p, `hub falloff col${x} row${y}`).not.toEqual(mark);
      }
    }
    for (const y of [5, 10]) {
      expect(pixel(img, 5, y), `hub vertical bound row${y}`).toEqual(plate);
      expect(pixel(img, 8, y), `hub vertical bound row${y}`).toEqual(plate);
    }

    // 轴上下出头：crispEdges 下 row1/row14 的轴列是满色，且这两行弧列为空。
    // 删掉 y=8/56 出头后这里会变回底板。
    for (const y of [1, 14]) {
      expect(pixel(img, 6, y), `axis cap row${y}`).toEqual(mark);
      expect(pixel(img, 11, y), `arc gap row${y}`).toEqual(plate);
    }

    // 轴独有行 ≥2（row1/2 与 row13/14）：轴有墨、弧列无墨。
    const axisOnlyRows = [1, 2, 13, 14].filter((y) => {
      const axisInk = !samePixel(pixel(img, 6, y), plate) || !samePixel(pixel(img, 7, y), plate);
      const arcInk = [9, 10, 11, 12].some((x) => !samePixel(pixel(img, x, y), plate));
      return axisInk && !arcInk;
    });
    expect(axisOnlyRows.length).toBeGreaterThanOrEqual(2);
  });
});
