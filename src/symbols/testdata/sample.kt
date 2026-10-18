package x

class Greeter(val n: String) {
    fun hello(): String = "hi"
}

interface Shape {
    fun area(): Double
}

object Registry {
    fun all() = 1
}

fun main() {}

typealias Name = String
