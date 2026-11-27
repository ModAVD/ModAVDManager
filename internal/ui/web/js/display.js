// Map a click through object-fit: contain, including portrait screenshots in a wide frame.
export function displayTap(rect, width, height, clientX, clientY) {
  if (width <= 0 || height <= 0 || rect.width <= 0 || rect.height <= 0) return null;
  const scale = Math.min(rect.width / width, rect.height / height);
  const shownWidth = width * scale, shownHeight = height * scale;
  const x = (clientX - rect.left - (rect.width - shownWidth) / 2) / shownWidth;
  const y = (clientY - rect.top - (rect.height - shownHeight) / 2) / shownHeight;
  if (x < 0 || x > 1 || y < 0 || y > 1) return null;
  return { type: 'tap', x: Math.round(x * (width - 1)), y: Math.round(y * (height - 1)) };
}
