interface Shape {
  area(): number
}

type ID = string

enum Color { Red }

namespace Util {
  export function helper() {}
}

abstract class Base {
  abstract run(): void
}

class Circle extends Base implements Shape {
  area(): number { return 0 }
  run() {}
}

export const make = (): Circle => new Circle()
