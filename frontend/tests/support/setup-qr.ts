import { readFileSync } from 'node:fs';

export const directSetupLink = 'https://phone.example.com/#setup=abcdef0123456789abcdef0123456789&label=Mac&relay=wss%3A%2F%2Fmac.example';
export const otherLink = 'https://example.com/';
export const gatewayInvitationLink = 'https://phone.example.com/#setup=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&invite=BBBBBBBBBBBBBBBBBBBBBBBB&invite_version=1&invite_expires=4102444800000&label=Test%20Workstation&gateways=wss%3A%2F%2Fnorth-america-gateway-one.example.com,wss%3A%2F%2Fnorth-america-gateway-two.example.com,wss%3A%2F%2Feurope-gateway-three.example.com,wss%3A%2F%2Fasia-pacific-gateway-four.example.com,wss%3A%2F%2Fgateway-five.example.com&relay_id=Ccy3nT9AULlAceTEnhTvoQ&rendezvous=xvT5VptkJHebIfy8b9PSGTJMkdRb-J_P2SXrtNRoLyA';

export function qrPixels(name: string, inverted = false) {
  const rows = readFileSync(`tests/fixtures/${name}.txt`, 'utf8').split('\n');
  const modules = rows[0].length - 2;
  const width = modules * 4;
  const data = new Uint8ClampedArray(width * width * 4);
  for (let y = 0; y < width; y += 1) {
    const row = rows[Math.floor(y / 8)];
    const half = y % 8 < 4 ? '▀' : '▄';
    for (let x = 0; x < width; x += 1) {
      const cell = row[Math.floor(x / 4) + 2];
      const filled = cell === '█' || cell === half;
      const grey = filled === inverted ? 255 : 0;
      data.set([grey, grey, grey, 255], (y * width + x) * 4);
    }
  }
  return { width, height: width, data };
}
