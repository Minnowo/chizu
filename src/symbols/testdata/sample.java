package x;

public class Greeter {
    public Greeter() {}

    public String hello() {
        return "hi";
    }
}

interface Shape {
    double area();
}

enum Color { RED }

record Point(int x) {}
