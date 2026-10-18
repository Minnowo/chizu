namespace Geo
{
    public class Greeter
    {
        public Greeter() {}
        public string Name { get; set; }
        public string Hello() { return "hi"; }
    }

    interface IShape { double Area(); }

    struct Point { public int X; }

    enum Color { Red }
}
