package x

class Greeter(n: String) {
  def hello(): String = "hi"
}

object Registry {
  def make(): Int = 1
}

trait Shape {
  def area: Double
}

enum Color { case Red }

type Name = String

def top(): Unit = ()
