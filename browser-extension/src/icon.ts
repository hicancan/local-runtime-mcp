// The toolbar L, drawn for its state: grey while lrmcp is away; in StuDev's gradient once connected; a blue dot where
// this site uses the gateway, a green one where it provides to it, both where it does both.
const BLUE = '#1F6FEB', GREEN = '#0E9F85';

function draw(size: number, connected: boolean, uses: boolean, provides: boolean): ImageData {
  const canvas = new OffscreenCanvas(size, size);
  const c = canvas.getContext('2d')!;
  const k = size / 512;
  const gradient = c.createLinearGradient(0, 0, size, size);
  if (connected) { gradient.addColorStop(0, '#00C6FF'); gradient.addColorStop(0.55, '#0A6CFF'); gradient.addColorStop(1, '#5B3FD8'); }
  else { gradient.addColorStop(0, '#B0B4BA'); gradient.addColorStop(1, '#80858C'); }
  c.strokeStyle = gradient; c.lineWidth = 88 * k * (size <= 16 ? 1.15 : 1); c.lineCap = 'round'; c.lineJoin = 'round';
  c.beginPath(); c.moveTo(206 * k, 140 * k); c.lineTo(206 * k, 346 * k); c.lineTo(326 * k, 346 * k); c.stroke();
  const dots = [uses && BLUE, provides && GREEN].filter(Boolean) as string[];
  const r = size * 0.19;
  dots.forEach((colour, i) => {
    const x = size - r - 0.5 - (dots.length - 1 - i) * r * 1.5, y = size - r - 0.5;
    c.beginPath(); c.arc(x, y, r + Math.max(1, size / 16), 0, Math.PI * 2); c.fillStyle = '#FFFFFF'; c.fill();
    c.beginPath(); c.arc(x, y, r, 0, Math.PI * 2); c.fillStyle = colour; c.fill();
  });
  return c.getImageData(0, 0, size, size);
}

export async function paintIcon(tabId: number | undefined, connected: boolean, uses: boolean, provides: boolean) {
  const imageData = { 16: draw(16, connected, uses, provides), 32: draw(32, connected, uses, provides) };
  try { await chrome.action.setIcon(tabId === undefined ? { imageData } : { tabId, imageData }); } catch { /* the tab is gone */ }
}
